package main

import (
	"io/fs"
	"net/rpc"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/lkarlslund/gonk"
)

type inodeinfo struct {
	dev, inode           uint64
	localhardlinkpath    string
	localdev, localinode uint64
	remaining            int32
}

func (i inodeinfo) Compare(i2 inodeinfo) int {
	d := i.dev - i2.dev
	if d != 0 {
		return int(d)
	}
	return int(i.inode - i2.inode)
}

type dirinfo struct {
	name         string
	info         FileInfo
	extraentries []string // files/folders that are local only, and should be deleted
	remaining    int32
}

func (f dirinfo) Compare(f2 dirinfo) int {
	return strings.Compare(f.name, f2.name)
}

type Client struct {
	BasePath string

	AlwaysChecksum bool
	SendACL        bool
	Delete         bool

	ParallelFile, ParallelDir int
	PreserveHardlinks         bool
	BlockSize                 int

	shutdown, done bool

	dirWorkerWG, fileWorkerWG sync.WaitGroup

	filequeue chan FileInfo
	inodes    gonk.Gonk[inodeinfo]

	dircache gonk.Gonk[dirinfo]

	dirstack    *stack[FileInfo]
	dirqueuein  chan<- FileInfo
	dirqueueout <-chan FileInfo
}

func NewClient() *Client {
	c := &Client{
		ParallelFile:      4096,
		ParallelDir:       512,
		PreserveHardlinks: true,
		BlockSize:         128 * 1024,
	}

	return c
}

func (c *Client) Run(client *rpc.Client) error {
	// Start the process
	var listfilesActive sync.WaitGroup

	c.dirstack, c.dirqueueout, c.dirqueuein = NewStack[FileInfo](c.ParallelDir*2, 8)
	c.filequeue = make(chan FileInfo, c.ParallelFile*16)

	// Check that remote path exists and we can connect to server
	var rootdirinfo FileInfo
	err := client.Call("Server.Stat", "/", &rootdirinfo)
	if err != nil {
		return err
	}
	logger.Debug().Msg("Queueing directory / from remote")
	listfilesActive.Add(1)
	c.dircache.Store(dirinfo{
		name:      rootdirinfo.Name,
		remaining: -1,
	})
	c.dirqueuein <- rootdirinfo

	// Launch directory workers
	for i := 0; i < c.ParallelDir; i++ {
		c.dirWorkerWG.Add(1)
		go func() {
			logger.Trace().Msg("Starting directory worker")
			for item := range c.dirqueueout {
				logger.Trace().Msgf("Processing directory queue item for %s", item.Name)

				var filelistresponse FileListResponse
				err := client.Call("Server.List", item.Name, &filelistresponse)

				logger.Trace().Msgf("Listfiles response for directory %v: %v entries", item.Name, len(filelistresponse.Files))
				if err != nil {
					logger.Error().Msgf("Error listing remote files in %v: %v", item.Name, err)
					continue
				}

				var filecount int
				remotenames := map[string]struct{}{}
				for _, remotefi := range filelistresponse.Files {
					remotenames[remotefi.Name] = struct{}{}
					if !remotefi.IsDir {
						filecount++
					}
				}

				var extraentries []string
				if c.Delete {
					localentries, err := os.ReadDir(filepath.Join(c.BasePath, item.Name))
					if err != nil {
						logger.Error().Msgf("Error listing local files in %v: %v", item.Name, err)
					} else {
						for _, le := range localentries {
							if _, found := remotenames[filepath.Join(item.Name, le.Name())]; !found {
								extraentries = append(extraentries, le.Name())
							}
						}
					}
				}

				processentries := len(filelistresponse.Files)
				logger.Trace().Msgf("Directory %v has %v remote entries (%v to delete local)", item.Name, len(filelistresponse.Files), len(extraentries))

				var directoryfound bool
				c.dircache.AtomicMutate(dirinfo{
					name: item.Name,
				}, func(f *dirinfo) {
					atomic.StoreInt32(&f.remaining, int32(processentries))
					f.extraentries = extraentries
					directoryfound = true
				}, false)
				if !directoryfound {
					logger.Error().Msgf("directory %v not found in directory cache", filelistresponse.ParentDirectory)
				}

				if processentries == 0 {
					// Handle it now
					logger.Trace().Msgf("No contents in folder %v detected", item.Name)
					c.ProcessedItemInDir(item.Name)
				} else {
					// queue files first
					for _, remotefi := range filelistresponse.Files {
						if !remotefi.IsDir {
							logger.Trace().Msgf("Queueing file %s", remotefi.Name)
							c.filequeue <- remotefi
						}
					}

					// queue directories second
					for _, remotefi := range filelistresponse.Files {
						if remotefi.IsDir {
							localpath := filepath.Join(c.BasePath, remotefi.Name)
							// logger.Trace().Msgf("Queueing directory %s", remotefi.Name)
							// check if directory exists
							localstat, err := PathToFileInfo(localpath)
							if os.IsNotExist(err) {
								logger.Trace().Msgf("Creating directory %s", localpath)
								err = os.MkdirAll(localpath, 0755)
								if err != nil {
									logger.Error().Msgf("Error creating directory %v: %v", localpath, err)
									continue
								}
							} else if err == nil {
								if !localstat.IsDir {
									logger.Debug().Msgf("Existing target for directory %v is not a directory, deleteing it", localpath)
									err = os.RemoveAll(localpath)
									if err != nil {
										logger.Error().Msgf("Error removing path %v: %v", localpath, err)
									}
									logger.Trace().Msgf("Creating directory %s", localpath)
									err = os.MkdirAll(localpath, 0755)
									if err != nil {
										logger.Error().Msgf("Error creating directory %v: %v", localpath, err)
										continue
									}
								}
							} else {
								logger.Warn().Msgf("Error getting information about path %v: %v", localpath, err)
							}
							logger.Trace().Msgf("Queueing directory %v", remotefi.Name)
							listfilesActive.Add(1)
							c.dircache.Store(dirinfo{
								name:      remotefi.Name,
								info:      remotefi,
								remaining: -1, // we don't know yet
							})
							c.dirqueuein <- remotefi
						}
					}
				}

				p.Add(DirectoriesProcessed, 1)
				listfilesActive.Done()
			}
			logger.Trace().Msg("Shutting down directory worker")
			c.dirWorkerWG.Done()
		}()
	}

	for i := 0; i < c.ParallelFile; i++ {
		c.fileWorkerWG.Add(1)
		go func() {
			logger.Trace().Msg("Starting file worker")
			for remotefi := range c.filequeue {
				localpath := filepath.Join(c.BasePath, remotefi.Name)
				logger.Trace().Msgf("Processing file %s", localpath)

				create_file := false
				copy_verify_file := false // do we need to copy it
				apply_attributes := false // do we need to update owner etc.

				localfi, err := PathToFileInfo(localpath)
				if err != nil {
					if os.IsNotExist(err) {
						logger.Debug().Msgf("File %s does not exist", localpath)
						localfi.Name = localpath
						create_file = true
					} else {
						logger.Error().Msgf("Error getting fileinfo for local path %s: %v", localpath, err)
						continue
					}
				}

				// Ignore it
				if !c.SendACL {
					localfi.ACL = nil
					remotefi.ACL = nil
				}

				remaininghardlinks := int32(-1) // not relevant
				var justaddedtoinodecache bool
				if c.PreserveHardlinks && remotefi.Nlink > 1 {
					logger.Trace().Msgf("Saving/updating remote dev/inode number %v/%v to cache for file %s with %d hardlinks", remotefi.Dev, remotefi.Inode, remotefi.Name, remotefi.Nlink)
					c.inodes.AtomicMutate(inodeinfo{
						dev:               remotefi.Dev,
						inode:             remotefi.Inode,
						localhardlinkpath: localpath,
						remaining:         int32(remotefi.Nlink),
					}, func(i *inodeinfo) {
						remaininghardlinks = atomic.AddInt32(&i.remaining, -1)
						if remaininghardlinks == int32(remotefi.Nlink-1) {
							logger.Trace().Msgf("Added file %s to inode cache", remotefi.Name)
							justaddedtoinodecache = true
						}
						if !create_file && atomic.CompareAndSwapUint64(&i.localinode, localfi.Inode, 0) {
							logger.Trace().Msgf("Updated local inode for file %s", remotefi.Name)
							atomic.SwapUint64(&i.localdev, localfi.Dev)
						}
					}, true)
				}

				// if it's a hardlinked file, check that it's linked correctly
				if !create_file && remotefi.Nlink > 1 && c.PreserveHardlinks && !justaddedtoinodecache {
					if ini, found := c.inodes.Load(inodeinfo{
						dev:   remotefi.Dev,
						inode: remotefi.Inode,
					}); found {
						if ini.localinode == 0 {
							// Find the local inode, we only need to do this once
							for {
								otherlocalfi, err := PathToFileInfo(ini.localhardlinkpath)
								if os.IsNotExist(err) {
									logger.Warn().Msgf("Local hardlink path %s does not exist, delaying a bit", ini.localhardlinkpath)
									time.Sleep(10 * time.Millisecond)
								} else if err != nil {
									logger.Error().Msgf("Error getting hardlink stat for local path %s: %v", ini.localhardlinkpath, err)
									continue
								} else {
									c.inodes.AtomicMutate(inodeinfo{
										dev:   remotefi.Dev,
										inode: remotefi.Inode,
									}, func(i *inodeinfo) {
										if atomic.CompareAndSwapUint64(&i.localinode, otherlocalfi.Inode, 0) {
											atomic.SwapUint64(&i.localdev, otherlocalfi.Dev)
											ini = *i
										}
									}, false)
									break
								}
							}
						}

						if localfi.Inode != ini.localinode || localfi.Dev != ini.localdev {
							logger.Debug().Msgf("Hardlink %s and %s have different inodes but should match, unlinking file", localpath, ini.localhardlinkpath)
							err = os.Remove(localpath)
							if err != nil {
								logger.Error().Msgf("Error unlinking %s: %v", localpath, err)
								continue
							}
							create_file = true
						}
					}
				}

				if !create_file && localfi.Mode&os.ModeType != remotefi.Mode&os.ModeType {
					logger.Debug().Msgf("File %s is indicating type change from %v to %v, unlinking", localpath, localfi.Mode.String(), remotefi.Mode.String())
					err = os.Remove(localpath)
					if err != nil {
						logger.Error().Msgf("Error unlinking %s: %v", localpath, err)
						continue
					}

					create_file = true
				}

				if !create_file { // still exists
					if localfi.Size > remotefi.Size && remotefi.Mode&fs.ModeSymlink == 0 {
						logger.Debug().Msgf("File %s is indicating size change from %v to %v, truncating", localpath, localfi.Size, remotefi.Size)
						err = os.Truncate(localpath, int64(remotefi.Size))
						if err != nil {
							logger.Error().Msgf("Error truncating %s to %v bytes to match remote: %v", localpath, remotefi.Size, err)
							continue
						}
						apply_attributes = true
					}
					if localfi.Mtim.Nano() != remotefi.Mtim.Nano() {
						logger.Debug().Msgf("File %s is indicating time change from %v to %v, applying attribute changes", localpath, time.Unix(0, localfi.Mtim.Nano()), time.Unix(0, remotefi.Mtim.Nano()))
						apply_attributes = true
					}

					if localfi.Mode.Perm() != remotefi.Mode.Perm() || localfi.Owner != remotefi.Owner || localfi.Group != remotefi.Group {
						logger.Debug().Msgf("File %s is indicating permissions changes, applying attribute changes", localpath)
						apply_attributes = true
					}

				}

				if create_file {
					apply_attributes = true
				}

				if remotefi.Size > 0 && remotefi.Mode&fs.ModeSymlink == 0 && (apply_attributes || c.AlwaysChecksum) {
					logger.Debug().Msgf("Doing file content validation for %s", localpath)
					copy_verify_file = true
				}

				// try to hard link it
				if create_file && c.PreserveHardlinks && remotefi.Nlink > 1 && !justaddedtoinodecache {
					if ini, found := c.inodes.Load(inodeinfo{
						dev:   remotefi.Dev,
						inode: remotefi.Inode,
					}); found {
						if localpath != ini.localhardlinkpath {
							logger.Debug().Msgf("Hardlinking %s to %s", localpath, ini.localhardlinkpath)
							var retries int
							for {
								err = os.Link(ini.localhardlinkpath, localpath)
								if err != nil {
									if os.IsNotExist(err) {
										retries++
										if retries < 25 {
											time.Sleep(100 * time.Millisecond)
											continue
										}
									}
									logger.Error().Msgf("Error hardlinking %s to %s: %v", localpath, ini.localhardlinkpath, err)
									break
								}
								break
							}
							if err != nil {
								continue
							}
							create_file = false
							copy_verify_file = false
							apply_attributes = true
						}
					} else {
						logger.Error().Msgf("Remote file %s indicates it should be hardlinked with %v others, but we don't have a match locally", remotefi.Name, remotefi.Nlink)
					}
				}

				transfersuccess := true

				if create_file {
					logger.Info().Msgf("Creating file %s", localpath)
				} else if copy_verify_file {
					logger.Info().Msgf("Updating/verifying file %s", localpath)
				} else if apply_attributes {
					logger.Info().Msgf("Applying attributes to file %s", localpath)
				}

				if create_file {
					err = localfi.Create(remotefi)
					if err == ErrNotSupportedByPlatform {
						logger.Warn().Msgf("Skipping %s: %v", localpath, err)
						continue
					} else if err != nil {
						logger.Error().Msgf("Error creating %s: %v", localpath, err)
						continue
					}
				}

				if copy_verify_file {
					// file exists but is different, copy it
					logger.Debug().Msgf("Processing blocks for %s", remotefi.Name)
					var existingsize int64

					// Open file if we didn't create it earlier
					localfile, err := os.OpenFile(localpath, os.O_RDWR, fs.FileMode(remotefi.Mode))
					if err != nil {
						logger.Error().Msgf("Error opening existing local file %s: %v", localpath, err)
						continue
					}
					fi, _ := os.Stat(localpath)
					existingsize = fi.Size()

					err = client.Call("Server.Open", remotefi.Name, nil)
					if err != nil {
						logger.Error().Msgf("Error opening remote file %s: %v", remotefi.Name, err)
						logger.Error().Msgf("Item fileinfo: %+v", remotefi)
						break
					}

					for i := int64(0); i < remotefi.Size; i += int64(c.BlockSize) {
						if !transfersuccess {
							break // we couldn't open the remote file
						}

						// Read the chunk
						length := int64(c.BlockSize)
						if i+length > remotefi.Size {
							length = remotefi.Size - i
						}
						chunkArgs := GetChunkArgs{
							Path:   remotefi.Name,
							Offset: uint64(i),
							Size:   uint64(length),
						}
						if i+length <= existingsize {
							var hash uint64
							err = client.Call("Server.ChecksumChunk", chunkArgs, &hash)
							if err != nil {
								logger.Error().Msgf("Error getting remote checksum for file %s chunk at %d: %v", remotefi.Name, i, err)
								transfersuccess = false
							}
							localdata := make([]byte, length)
							n, err := localfile.ReadAt(localdata, i)
							if err != nil {
								logger.Error().Msgf("Error reading existing local file %s chunk at %d: %v", localpath, i, err)
								transfersuccess = false
								break
							}
							p.Add(ReadBytes, uint64(length))
							if n == int(length) {
								localhash := xxhash.Sum64(localdata)
								logger.Trace().Msgf("Checksum for file %s chunk at %d is %X, remote is %X", remotefi.Name, i, localhash, hash)
								if localhash == hash {
									continue // Block matches
								}
							}
						}

						var data []byte
						logger.Debug().Msgf("Transferring file %s chunk at %d", remotefi.Name, i)
						err = client.Call("Server.GetChunk", chunkArgs, &data)
						if err != nil {
							logger.Error().Msgf("Error transferring file %s chunk at %d: %v", remotefi.Name, i, err)
							transfersuccess = false
							break
						}
						n, err := localfile.WriteAt(data, i)
						if err != nil {
							logger.Error().Msgf("Error writing to local file %s chunk at %d: %v", localpath, i, err)
							transfersuccess = false
							break
						}
						if n != int(length) {
							logger.Error().Msgf("Wrote %v bytes but expected to write %v", n, length)
							transfersuccess = false
							break
						}
						p.Add(WrittenBytes, uint64(length))
						apply_attributes = true
					}
					err = client.Call("Server.Close", remotefi.Name, nil)
					if err != nil {
						logger.Error().Msgf("Error closing remote file %s: %v", remotefi.Name, err)
					}
					localfile.Close()
				}

				if apply_attributes && transfersuccess {
					logger.Debug().Msgf("Updating metadata for %s", remotefi.Name)
					err = localfi.ApplyChanges(remotefi)
					if err != nil {
						logger.Error().Msgf("Error applying metadata for %s: %v", remotefi.Name, err)
					}
				}

				// handle inode counters
				if remaininghardlinks == 0 {
					// No more references, free up some memory
					logger.Trace().Msgf("No more references to this inode, removing it from inode cache")
					c.inodes.Delete(inodeinfo{
						dev:   remotefi.Dev,
						inode: remotefi.Inode,
					})
				}

				// are we done with this directory, the apply attributes to that
				c.ProcessedItemInDir(filepath.Dir(remotefi.Name))
				p.Add(FilesProcessed, 1)
				p.Add(BytesProcessed, uint64(remotefi.Size))
			}
			logger.Trace().Msg("Shutting down file worker")
			c.fileWorkerWG.Done()
		}()
	}

	// wait for all directories to be listed
	listfilesActive.Wait()
	logger.Debug().Msg("No more directories to list")
	// close the directory stack
	c.dirstack.Close()
	// wait for all directory workers to finish
	c.dirWorkerWG.Wait()
	// close the file queue so file workers can finish
	close(c.filequeue)
	// wait for all workers to finish
	c.fileWorkerWG.Wait()

	logger.Debug().Msg("Client routine done")

	return nil
}

func (c *Client) ProcessedItemInDir(path string) {
	lookupdirectory := dirinfo{
		name: path,
	}
	donewithdirectory := false
	founddirectory := false
	c.dircache.AtomicMutate(lookupdirectory, func(item *dirinfo) {
		founddirectory = true
		left := atomic.AddInt32(&item.remaining, -1)
		logger.Trace().Msgf("directory %s has usage %v left", item.name, left)
		if left <= 0 { // zero for folders with contents, -1 for blank folders
			c.PostProcessDir(item)
			donewithdirectory = true // delete operation must be outside this atomic operation
		}
	}, false)
	if !founddirectory {
		logger.Error().Msgf("Failed to find directory info for postprocessing %s", lookupdirectory.name)
	}
	if donewithdirectory {
		c.dircache.Delete(lookupdirectory)
		if path != "/" {
			c.ProcessedItemInDir(filepath.Dir(path))
		}
	}
}

func (c *Client) PostProcessDir(item *dirinfo) {
	if c.Delete {
		for _, extraentry := range item.extraentries {
			err := os.RemoveAll(filepath.Join(c.BasePath, item.name, extraentry))
			if err != nil {
				logger.Error().Msgf("Error unlinking %v: %v", filepath.Join(c.BasePath, item.name, extraentry), err)
			}
			p.Add(EntriesDeleted, 1)
		}
	}

	// Apply modify times to directory
	localdirfi, err := PathToFileInfo(filepath.Join(c.BasePath, item.name))
	if err != nil {
		logger.Error().Msgf("Problem getting local directory information for %v: %v", filepath.Join(c.BasePath, item.name), err)
	} else {
		localdirfi.ApplyChanges(item.info)
	}
}

func (c *Client) Abort() {
	c.shutdown = true
}

func (c *Client) Done() bool {
	return c.done
}

func (c *Client) Stats() (inodes, directories, filequeue, directoriestack int) {
	return c.inodes.Len(),
		c.dircache.Len(),
		len(c.filequeue),
		c.dirstack.Len()
}

func (c *Client) Wait() {
	c.fileWorkerWG.Wait()
}

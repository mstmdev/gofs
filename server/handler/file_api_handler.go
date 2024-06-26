package handler

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/no-src/gofs/contract"
	"github.com/no-src/gofs/logger"
	"github.com/no-src/gofs/server"
	"github.com/no-src/nsgo/fsutil"
	"github.com/no-src/nsgo/hashutil"
)

type fileApiHandler struct {
	logger          *logger.Logger
	root            http.Dir
	chunkSize       int64
	checkpointCount int
	hash            hashutil.Hash
}

// NewFileApiHandlerFunc returns a gin.HandlerFunc that queries the file info
func NewFileApiHandlerFunc(logger *logger.Logger, root http.Dir, chunkSize int64, checkpointCount int, hash hashutil.Hash) gin.HandlerFunc {
	return (&fileApiHandler{
		logger:          logger,
		root:            root,
		chunkSize:       chunkSize,
		checkpointCount: checkpointCount,
		hash:            hash,
	}).Handle
}

func (h *fileApiHandler) Handle(c *gin.Context) {
	defer func() {
		e := recover()
		if e != nil {
			c.JSON(http.StatusOK, server.NewServerErrorResult())
		}
	}()

	var fileList []contract.FileInfo
	path := c.Query(contract.FsPath)
	needHash := c.Query(contract.FsNeedHash) == contract.FsNeedHashValueTrue
	needCheckpoint := c.Query(contract.FsNeedCheckpoint) == contract.ParamValueTrue

	sourcePrefix := strings.Trim(server.SourceRoutePrefix, "/")
	destPrefix := strings.Trim(server.DestRoutePrefix, "/")
	if !strings.HasPrefix(strings.ToLower(path), sourcePrefix) && !strings.HasPrefix(strings.ToLower(path), destPrefix) {
		c.JSON(http.StatusOK, server.NewErrorApiResult(-501, "must start with source or dest"))
		return
	}

	path = filepath.Clean(path)
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(strings.ToLower(path), sourcePrefix) && !strings.HasPrefix(strings.ToLower(path), destPrefix) {
		c.JSON(http.StatusOK, server.NewErrorApiResult(-502, "invalid path"))
		return
	}

	path = strings.TrimLeft(path, sourcePrefix)

	f, err := h.root.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			h.logger.Error(err, contract.NotFoundDesc)
			c.JSON(http.StatusOK, server.NewErrorApiResult(contract.NotFound, contract.NotFoundDesc))
		} else {
			h.logger.Error(err, "file server open path error")
			c.JSON(http.StatusOK, server.NewErrorApiResult(-503, "open path error"))
		}
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		h.logger.Error(err, "file server get file stat error")
		c.JSON(http.StatusOK, server.NewErrorApiResult(-504, "get file stat error"))
		return
	}

	if stat.IsDir() {
		dirFileList, err := h.readDir(f, needHash, needCheckpoint, path)
		if err != nil {
			c.JSON(http.StatusOK, server.NewErrorApiResult(-505, "read dir error"))
			return
		}
		fileList = append(fileList, dirFileList...)
	}

	c.JSON(http.StatusOK, server.NewApiResult(contract.Success, contract.SuccessDesc, fileList))
}

func (h *fileApiHandler) readDir(f http.File, needHash bool, needCheckpoint bool, path string) (fileList []contract.FileInfo, err error) {
	const (
		maxCalcSizeSingle int64 = 1024 * 1024 * 1024 * 15  // 15G
		maxCalcSizeSum    int64 = 1024 * 1024 * 1024 * 500 // 500G
	)
	var calcSizeSum int64
	files, err := f.Readdir(-1)
	if err != nil {
		h.logger.Error(err, "file server read dir error")
		return fileList, err
	}
	for _, file := range files {
		cTime, aTime, mTime, fsTimeErr := fsutil.GetFileTimeBySys(file.Sys())
		if fsTimeErr != nil {
			h.logger.Error(fsTimeErr, "get file times error => %s", file.Name())
			cTime = time.Now()
			aTime = cTime
			mTime = cTime
		}

		hash := ""
		var hvs hashutil.HashValues
		if !file.IsDir() && (needHash || needCheckpoint) && calcSizeSum < maxCalcSizeSum && file.Size() < maxCalcSizeSingle {
			if cf, err := h.root.Open(filepath.ToSlash(filepath.Join(path, file.Name()))); err == nil {
				if needCheckpoint {
					hvs, _ = h.hash.CheckpointsHashFromFile(cf.(*os.File), h.chunkSize, h.checkpointCount)
				}
				if needHash {
					if len(hvs) > 0 {
						hash = hvs.Last().Hash
					} else {
						hash, _ = h.hash.HashFromFile(cf)
					}
				}
				cf.Close()
			}
			calcSizeSum += file.Size()
		}

		fileList = append(fileList, contract.FileInfo{
			Path:       file.Name(),
			IsDir:      contract.ParseFsDirValue(file.IsDir()),
			Size:       file.Size(),
			Hash:       hash,
			HashValues: hvs,
			ATime:      aTime.Unix(),
			CTime:      cTime.Unix(),
			MTime:      mTime.Unix(),
			LinkTo:     h.readlink(file),
		})
	}
	return fileList, nil
}

func (h *fileApiHandler) readlink(file fs.FileInfo) string {
	if fsutil.IsSymlinkMode(file.Mode()) {
		path := filepath.Join(string(h.root), file.Name())
		realPath, err := fsutil.Readlink(path)
		if err == nil {
			return realPath
		}
		h.logger.Error(err, "file api read link error => %s", path)
		return file.Name()
	}
	return ""
}

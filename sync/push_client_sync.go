package sync

import (
	"errors"
	"fmt"
	"github.com/no-src/gofs/action"
	"github.com/no-src/gofs/auth"
	"github.com/no-src/gofs/contract"
	"github.com/no-src/gofs/contract/push"
	"github.com/no-src/gofs/core"
	"github.com/no-src/gofs/fs"
	"github.com/no-src/gofs/ignore"
	"github.com/no-src/gofs/server"
	"github.com/no-src/gofs/server/client"
	"github.com/no-src/gofs/tran"
	"github.com/no-src/gofs/util"
	"github.com/no-src/log"
	"io"
	iofs "io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type pushClientSync struct {
	diskSync
	source          core.VFS
	dest            core.VFS
	sourceAbsPath   string
	pushAddr        string
	cookies         []*http.Cookie
	currentUser     *auth.User
	currentHashUser *auth.HashUser
	client          tran.Client
	authChan        chan contract.Status
	infoChan        chan contract.Message
}

const timeout = time.Minute * 3

func NewPushClientSync(source, dest core.VFS, enableTLS bool, users []*auth.User, enableLogicallyDelete bool) (Sync, error) {
	ds, err := newDiskSync(source, dest, enableLogicallyDelete)
	if err != nil {
		return nil, err
	}

	sourceAbsPath, err := source.Abs()
	if err != nil {
		return nil, err
	}

	s := &pushClientSync{
		source:        source,
		dest:          dest,
		sourceAbsPath: sourceAbsPath,
		diskSync:      *ds,
		client:        tran.NewClient(dest.Host(), dest.Port(), enableTLS),
		authChan:      make(chan contract.Status, 100),
		infoChan:      make(chan contract.Message, 100),
	}

	if len(users) > 0 {
		user := users[0]
		hashUser, err := user.ToHashUser()
		if err != nil {
			return nil, err
		}
		s.currentUser = user
		s.currentHashUser = hashUser
	}

	err = s.start()
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (pcs *pushClientSync) start() error {
	err := pcs.client.Connect()
	if err != nil {
		return err
	}
	pcs.receive()
	err = pcs.auth()
	if err != nil {
		return err
	}
	err = pcs.info()
	if err == nil {
		pcs.client.Close()
	}
	return err
}

func (pcs *pushClientSync) auth() error {
	// if the currentHashUser is nil, it means to anonymous access
	if pcs.currentHashUser == nil {
		return nil
	}
	go func() {
		pcs.currentHashUser.RefreshExpires()
		authData := auth.GenerateAuthCommandData(pcs.currentHashUser)
		err := pcs.client.Write(authData)
		if err != nil {
			log.Error(err, "send auth request error")
		}
	}()

	var status contract.Status
	select {
	case status = <-pcs.authChan:
	case <-time.After(timeout):
		return fmt.Errorf("auth timeout for %s", timeout.String())
	}
	if status.Code != contract.Success {
		return errors.New("receive auth command response error => " + status.Message)
	}

	log.Info("auth success, current client is authorized => [%s] ", status.Message)
	return nil
}

func (pcs *pushClientSync) info() error {
	go func() {
		if err := pcs.client.Write(contract.InfoCommand); err != nil {
			log.Error(err, "write info command error")
		}
	}()
	var info contract.FileServerInfo
	var infoMsg contract.Message
	select {
	case infoMsg = <-pcs.infoChan:
	case <-time.After(timeout):
		return fmt.Errorf("info timeout for %s", timeout.String())
	}
	err := util.Unmarshal(infoMsg.Data, &info)
	if err != nil {
		return err
	}

	if info.Code != contract.Success {
		return errors.New("receive info command response error => " + info.Message)
	}
	pcs.pushAddr = info.ServerAddr + info.PushAddr
	return nil
}

func (pcs *pushClientSync) receive() {
	go func() {
		for {
			if pcs.client.IsClosed() {
				break
			}
			data, err := pcs.client.ReadAll()
			if err != nil {
				if pcs.client.IsClosed() {
					break
				} else {
					log.Error(err, "[push client sync] read data error")
				}
			} else {
				var status contract.Status
				err = util.Unmarshal(data, &status)
				if err != nil {
					log.Error(err, "[push client sync] unmarshal data error")
					continue
				}
				switch status.ApiType {
				case contract.AuthApi:
					pcs.authChan <- status
					break
				case contract.InfoApi:
					pcs.infoChan <- contract.NewMessage(data)
					break
				default:
					log.Warn("[push client sync] receive and discard data => %s", string(data))
					break
				}
			}
		}
	}()
}

func (pcs *pushClientSync) Create(path string) error {
	if !pcs.dest.LocalSyncDisabled() {
		if err := pcs.diskSync.Create(path); err != nil {
			return err
		}
	}
	return pcs.send(action.CreateAction, path)
}

func (pcs *pushClientSync) Write(path string) error {
	if !pcs.dest.LocalSyncDisabled() {
		if err := pcs.diskSync.Write(path); err != nil {
			return err
		}
	}
	isDir, err := pcs.IsDir(path)
	if err != nil {
		return err
	}
	if isDir {
		return pcs.SyncOnce(path)
	} else {
		return pcs.send(action.WriteAction, path)
	}
}

func (pcs *pushClientSync) Remove(path string) error {
	if !pcs.dest.LocalSyncDisabled() {
		if err := pcs.diskSync.Remove(path); err != nil {
			return err
		}
	}
	return pcs.send(action.RemoveAction, path)
}

func (pcs *pushClientSync) Rename(path string) error {
	if !pcs.dest.LocalSyncDisabled() {
		if err := pcs.diskSync.Remove(path); err != nil {
			return err
		}
	}
	return pcs.send(action.RenameAction, path)
}

func (pcs *pushClientSync) Chmod(path string) error {
	log.Debug("Chmod is unimplemented [%s]", path)
	return nil
}

func (pcs *pushClientSync) IsDir(path string) (bool, error) {
	return pcs.diskSync.IsDir(path)
}

func (pcs *pushClientSync) SyncOnce(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	return filepath.WalkDir(absPath, func(currentPath string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ignore.MatchPath(currentPath, "push client sync", "sync once") {
			return nil
		}
		if d.IsDir() {
			err = pcs.Create(currentPath)
		} else {
			err = pcs.Create(currentPath)
			if err == nil {
				err = pcs.Write(currentPath)
			}
		}
		return err
	})
}

func (pcs *pushClientSync) Source() core.VFS {
	return pcs.source
}

func (pcs *pushClientSync) Dest() core.VFS {
	return pcs.dest
}

func (pcs *pushClientSync) send(act action.Action, path string) (err error) {
	isDir := false
	if act != action.RemoveAction && act != action.RenameAction {
		isDir, err = pcs.IsDir(path)
		if err != nil {
			return err
		}
	}

	var size int64
	hash := ""
	cTime := time.Now()
	aTime := time.Now()
	mTime := time.Now()
	if !isDir && act == action.WriteAction {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		fileInfo, err := file.Stat()
		if err != nil {
			return err
		}
		size = fileInfo.Size()
		if size > 0 {
			hash, err = util.MD5FromFile(file)
			if err != nil {
				return err
			}
		}
	} else if isDir && act == action.WriteAction {
		return nil
	}

	if act == action.WriteAction || act == action.CreateAction {
		var timeErr error
		cTime, aTime, mTime, timeErr = fs.GetFileTime(path)
		if timeErr != nil {
			return timeErr
		}
	}

	isDirValue := contract.FsUnknown
	if isDir {
		isDirValue = contract.FsIsDir
	} else {
		isDirValue = contract.FsNotDir
	}

	relPath, err := filepath.Rel(pcs.sourceAbsPath, path)
	if err != nil {
		return err
	}
	relPath = filepath.ToSlash(relPath)
	req := push.PushData{
		Action: act,
		FileInfo: contract.FileInfo{
			Path:  relPath,
			IsDir: isDirValue,
			Size:  size,
			Hash:  hash,
			CTime: cTime.Unix(),
			ATime: aTime.Unix(),
			MTime: mTime.Unix(),
		},
	}

	data, err := util.Marshal(req)
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set(push.FileInfo, string(data))
	resp, err := pcs.httpPostWithAuth(pcs.pushAddr, act, push.UpFile, path, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var apiResult server.ApiResult
	err = util.Unmarshal(respData, &apiResult)
	if err != nil {
		return err
	}
	if apiResult.Code != contract.Success {
		err = fmt.Errorf("send a request to the push server error => %s", apiResult.Message)
	}
	return err
}

func (pcs *pushClientSync) httpPostWithAuth(rawURL string, act action.Action, fieldName, fileName string, data url.Values) (resp *http.Response, err error) {
	sendFile := false
	if act == action.WriteAction {
		sendFile = true
	}
	if sendFile {
		resp, err = util.HttpPostFileWithCookie(rawURL, fieldName, fileName, data, pcs.cookies...)
	} else {
		resp, err = util.HttpPostWithCookie(rawURL, data, pcs.cookies...)
	}

	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && pcs.currentUser != nil {
		// auto login
		parseUrl, err := url.Parse(rawURL)
		if err != nil {
			return nil, err
		}
		user := pcs.currentUser
		cookies, err := client.SignIn(parseUrl.Scheme, parseUrl.Host, user.UserName(), user.Password())
		if err != nil {
			return nil, err
		}
		if len(cookies) > 0 {
			pcs.cookies = cookies
			log.Debug("try to auto login file server success maybe, retry to get resource => %s", rawURL)
			if sendFile {
				return util.HttpPostFileWithCookie(rawURL, fieldName, fileName, data, pcs.cookies...)
			} else {
				return util.HttpPostWithCookie(rawURL, data, pcs.cookies...)
			}
		}
		return nil, errors.New("file server is unauthorized")
	} else if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("the push server is unsupported => %s", rawURL)
	}
	return resp, err
}
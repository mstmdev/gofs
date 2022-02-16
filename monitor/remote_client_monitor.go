package monitor

import (
	"errors"
	"fmt"
	"github.com/no-src/gofs/action"
	"github.com/no-src/gofs/auth"
	"github.com/no-src/gofs/contract"
	"github.com/no-src/gofs/eventlog"
	"github.com/no-src/gofs/ignore"
	"github.com/no-src/gofs/internal/cbool"
	"github.com/no-src/gofs/internal/clist"
	"github.com/no-src/gofs/retry"
	"github.com/no-src/gofs/sync"
	"github.com/no-src/gofs/tran"
	"github.com/no-src/gofs/util"
	"github.com/no-src/gofs/wait"
	"github.com/no-src/log"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

type remoteClientMonitor struct {
	baseMonitor
	client      tran.Client
	closed      *cbool.CBool
	messages    *clist.CList
	currentUser *auth.HashUser
	authorized  bool
	authChan    chan contract.Status
	infoChan    chan contract.Message
}

const timeout = time.Minute * 3

// NewRemoteClientMonitor create an instance of remoteClientMonitor to monitor the remote file change
func NewRemoteClientMonitor(syncer sync.Sync, retry retry.Retry, syncOnce bool, host string, port int, enableTLS bool, users []*auth.User, eventWriter io.Writer) (Monitor, error) {
	if syncer == nil {
		err := errors.New("syncer can't be nil")
		return nil, err
	}
	m := &remoteClientMonitor{
		client:      tran.NewClient(host, port, enableTLS),
		messages:    clist.New(),
		baseMonitor: newBaseMonitor(syncer, retry, syncOnce, eventWriter),
		authChan:    make(chan contract.Status, 100),
		infoChan:    make(chan contract.Message, 100),
		closed:      cbool.New(false),
	}
	if len(users) > 0 {
		user := users[0]
		hashUser, err := user.ToHashUser()
		if err != nil {
			return nil, err
		}
		m.currentUser = hashUser
	}
	return m, nil
}

// auth send auth request
func (m *remoteClientMonitor) auth() error {
	// if the currentUser is nil, it means to anonymous access
	if m.currentUser == nil {
		return nil
	}
	go m.retry.Do(func() error {
		m.currentUser.RefreshExpires()
		authData := auth.GenerateAuthCommandData(m.currentUser)
		err := m.client.Write(authData)
		return err
	}, "send auth request")

	var status contract.Status
	select {
	case status = <-m.authChan:
	case <-time.After(timeout):
		return fmt.Errorf("auth timeout for %s", timeout.String())
	}
	if status.Code != contract.Success {
		return errors.New("receive auth command response error => " + status.Message)
	}

	// auth success
	m.authorized = true
	log.Info("auth success, current client is authorized => [%s] ", status.Message)
	return nil
}

func (m *remoteClientMonitor) Start() error {
	if m.client == nil {
		return errors.New("remote sync client is nil")
	}
	// connect -> auth -> info|read
	err := m.client.Connect()
	if err != nil {
		return err
	}

	w := m.receive()

	if err = m.auth(); err != nil {
		return err
	}

	// execute -sync_once flag
	if m.syncOnce {
		return m.syncAndWaitShutdown(w)
	}

	// execute -sync_cron flag
	if err := m.startCron(m.sync); err != nil {
		return err
	}

	go m.processWrite()
	go m.startSyncWrite()
	go m.processingMessage()

	return w.Wait()
}

func (m *remoteClientMonitor) sync() (err error) {
	go func() {
		if err := m.client.Write(contract.InfoCommand); err != nil {
			log.Error(err, "write info command error")
		}
	}()
	var info contract.FileServerInfo
	var infoMsg contract.Message
	select {
	case infoMsg = <-m.infoChan:
	case <-time.After(timeout):
		return fmt.Errorf("sync timeout for %s", timeout.String())
	}
	err = util.Unmarshal(infoMsg.Data, &info)
	if err != nil {
		return err
	}

	if info.Code != contract.Success {
		return errors.New("receive info command response error => " + info.Message)
	}

	return m.syncer.SyncOnce(info.ServerAddr + info.SourcePath)
}

func (m *remoteClientMonitor) syncAndWaitShutdown(w wait.Wait) (err error) {
	if err = m.sync(); err != nil {
		return err
	}
	if err = m.Shutdown(); err != nil {
		return err
	}
	return w.Wait()
}

func (m *remoteClientMonitor) receive() wait.Wait {
	wd := wait.NewWaitDone()
	shutdown := cbool.New(false)
	go func() {
		select {
		case <-shutdown.SetC(<-m.shutdown):
			{
				if shutdown.Get() {
					if err := m.Close(); err != nil {
						log.Error(err, "close remote client monitor error")
					}
					wd.Done()
				}
			}
		}
	}()
	go func() {
		for {
			if m.closed.Get() {
				wd.DoneWithError(errors.New("remote monitor is closed"))
				break
			}
			data, err := m.client.ReadAll()
			if err != nil {
				if shutdown.Get() {
					break
				}
				log.Error(err, "remote client monitor read data error")
				if m.client.IsClosed() {
					m.authorized = false
					log.Debug("try reconnect to server %s:%d", m.client.Host(), m.client.Port())
					m.retry.Do(func() error {
						if m.client.IsClosed() {
							innerErr := m.client.Connect()
							if innerErr != nil {
								return innerErr
							}
						}
						return nil
					}, fmt.Sprintf("client reconnect to %s:%d", m.client.Host(), m.client.Port()))

					if !m.authorized {
						go m.auth()
					}
				}
			} else {
				var status contract.Status
				err = util.Unmarshal(data, &status)
				if err != nil {
					log.Error(err, "remote client monitor unmarshal data error")
					continue
				}

				switch status.ApiType {
				case contract.AuthApi:
					m.authChan <- status
					break
				case contract.InfoApi:
					m.infoChan <- contract.NewMessage(data)
					break
				case contract.SyncMessageApi:
					m.messages.PushBack(contract.NewMessage(data))
					break
				default:
					log.Warn("remote client monitor received a unknown data => %s", string(data))
					break
				}
			}
		}
	}()
	return wd
}

func (m *remoteClientMonitor) processingMessage() {
	for {
		element := m.messages.Front()
		if element == nil || element.Value == nil {
			if element != nil {
				m.messages.Remove(element)
			}
			<-time.After(time.Second)
			continue
		}
		message := element.Value.(contract.Message)
		log.Info("client read request => %s", message.String())
		var msg sync.Message
		err := util.Unmarshal(message.Data, &msg)
		if err != nil {
			log.Error(err, "client unmarshal data error")
		} else if msg.Code != contract.Success {
			log.Error(errors.New(msg.Message), "remote monitor received the error message")
		} else if ignore.MatchPath(msg.Path, "remote client monitor", msg.Action.String()) {
			// ignore match
		} else {
			values := url.Values{}
			values.Add(contract.FsDir, msg.IsDir.String())
			values.Add(contract.FsSize, util.String(msg.Size))
			values.Add(contract.FsHash, msg.Hash)
			values.Add(contract.FsCtime, util.String(msg.CTime))
			values.Add(contract.FsAtime, util.String(msg.ATime))
			values.Add(contract.FsMtime, util.String(msg.MTime))

			// replace question marks with "%3F" to avoid parse the path is breaking when it contains some question marks
			path := msg.BaseUrl + strings.ReplaceAll(msg.Path, "?", "%3F") + fmt.Sprintf("?%s", values.Encode())

			switch msg.Action {
			case action.CreateAction:
				err = m.syncer.Create(path)
				break
			case action.WriteAction:
				err = m.syncer.Create(path)
				// ignore is not exist error
				if err != nil && os.IsNotExist(err) {
					err = nil
				}
				m.addWrite(path)
				break
			case action.RemoveAction:
				m.removeWrite(path)
				err = m.syncer.Remove(path)
				break
			case action.RenameAction:
				err = m.syncer.Rename(path)
				break
			case action.ChmodAction:
				err = m.syncer.Chmod(path)
				break
			}

			m.el.Write(eventlog.NewEvent(path, msg.Action.String()))

			if err != nil {
				log.Error(err, "%s action execute error => [%s]", msg.Action.String(), path)
			}
		}
		m.messages.Remove(element)
	}
}

func (m *remoteClientMonitor) Close() error {
	m.closed.Set(true)
	if m.client != nil {
		return m.client.Close()
	}
	return nil
}

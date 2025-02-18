package monitor

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/no-src/gofs/action"
	"github.com/no-src/gofs/api/apiclient"
	"github.com/no-src/gofs/api/monitor"
	"github.com/no-src/gofs/auth"
	"github.com/no-src/gofs/contract"
	"github.com/no-src/gofs/eventlog"
	"github.com/no-src/gofs/ignore"
	"github.com/no-src/gofs/internal/clist"
	"github.com/no-src/gofs/wait"
	"github.com/no-src/nsgo/fsutil"
	"github.com/no-src/nsgo/stringutil"
)

type remoteClientMonitor struct {
	baseMonitor

	client   apiclient.Client
	closed   atomic.Bool
	messages *clist.CList
	pi       ignore.PathIgnore
}

// NewRemoteClientMonitor create an instance of remoteClientMonitor to monitor the remote file change
func NewRemoteClientMonitor(opt Option) (Monitor, error) {
	source := opt.Syncer.Source()
	syncer := opt.Syncer
	host := source.Host()
	port := source.Port()
	enableTLS := opt.EnableTLS
	certFile := opt.TLSCertFile
	users := opt.Users
	pi := opt.PathIgnore

	if syncer == nil {
		err := errors.New("syncer can't be nil")
		return nil, err
	}

	var user *auth.User
	if len(users) > 0 {
		user = users[0]
	}
	m := &remoteClientMonitor{
		client:      apiclient.New(host, port, enableTLS, certFile, user),
		messages:    clist.New(),
		baseMonitor: newBaseMonitor(opt),
		pi:          pi,
	}
	return m, nil
}

func (m *remoteClientMonitor) Start() (wait.Wait, error) {
	if m.client == nil {
		return nil, errors.New("remote sync client is nil")
	}
	err := m.client.Start()
	if err != nil {
		return nil, err
	}

	w := m.receive()

	// execute -sync_once flag
	if m.syncOnce {
		return w, m.syncAndShutdown()
	}

	// execute -sync_cron flag
	if err := m.startCron(m.sync); err != nil {
		return nil, err
	}

	go m.startReceiveWriteNotify()
	go m.startSyncWrite()
	go m.startProcessMessage()

	return w, nil
}

// sync try to sync all the files once
func (m *remoteClientMonitor) sync() (err error) {
	info, err := m.client.GetInfo()
	if err != nil {
		return err
	}
	return m.syncer.SyncOnce(info.ServerAddr + info.SourcePath)
}

// syncAndShutdown execute sync and then try to shut down, the caller should wait for shutdown by wait.Wait()
func (m *remoteClientMonitor) syncAndShutdown() (err error) {
	if err = m.sync(); err != nil {
		return err
	}
	if err = m.Shutdown(); err != nil {
		return err
	}
	return nil
}

// receive start receiving messages and parse the message, send to consumers according to the api type.
// if receive a shutdown notify, then stop reading the message.
func (m *remoteClientMonitor) receive() wait.Wait {
	wd := wait.NewWaitDone()
	shutdown := &atomic.Bool{}
	go m.waitShutdown(shutdown, wd)
	go m.readMessage(shutdown, wd)
	return wd
}

// waitShutdown wait for the shutdown notify then mark the work done
func (m *remoteClientMonitor) waitShutdown(st *atomic.Bool, wd wait.Done) {
	<-m.shutdown
	st.Store(true)
	m.logger.ErrorIf(m.Close(), "close remote client monitor error")
	m.syncer.Close()
	wd.Done()
}

// readMessage loop read the messages, if receive a message, parse the message then send to consumers according to the api type.
// if receive a shutdown notify, then stop reading the message.
func (m *remoteClientMonitor) readMessage(st *atomic.Bool, wd wait.Done) {
	mc, err := m.client.Monitor()
	if err != nil {
		return
	}
	errCount := 0
	errThreshold := 20
	for {
		if m.closed.Load() {
			wd.DoneWithError(errors.New("remote monitor is closed"))
			break
		}
		msg, err := mc.Recv()
		if err != nil {
			if st.Load() {
				break
			}
			errCount++
			m.logger.Error(err, "receive monitor message error")
			if m.client.IsClosed(err) {
				m.retry.Do(func() error {
					nmc, err := m.client.Monitor()
					if err == nil {
						mc = nmc
						m.logger.Info("monitor the remote server success")
					}
					return err
				}, "monitor the remote server")
			} else if m.client.IsUnauthenticated(err) {
				if m.logger.ErrorIf(m.client.Login(), "re-login to remote server error") == nil {
					m.logger.Info("re-login to remote server success")
					if nmc, err := m.client.Monitor(); err == nil {
						mc = nmc
						m.logger.Info("monitor the remote server success")
					}
				}
			} else if errCount > errThreshold {
				w := min(errCount/errThreshold, errThreshold)
				m.logger.Info("receive monitor message error threshold exceeded, will retry after %d seconds", w)
				time.Sleep(time.Second * time.Duration(w))
			}
		} else {
			errCount = 0
			m.messages.PushBack(msg)
		}
	}
}

// startProcessMessage start loop to process the file change messages
func (m *remoteClientMonitor) startProcessMessage() {
	for {
		m.waitSyncDelay(m.messages.Len)

		element := m.messages.Front()
		if element == nil || element.Value == nil {
			if element != nil {
				m.messages.Remove(element)
			}
			m.resetSyncDelay()
			<-time.After(time.Second)
			continue
		}
		msg := element.Value.(*monitor.MonitorMessage)
		m.logger.Info("client read request => %s", msg.String())
		if m.pi.MatchPath(msg.FileInfo.Path, "remote client monitor", action.Action(msg.Action).String()) {
			// ignore match
		} else {
			m.execSync(msg)
		}
		m.messages.Remove(element)
	}
}

// execSync execute the file change message to sync
func (m *remoteClientMonitor) execSync(msg *monitor.MonitorMessage) (err error) {
	fi := msg.FileInfo
	values := url.Values{}
	values.Add(contract.FsDir, contract.FsDirValue(fi.IsDir).String())
	values.Add(contract.FsSize, stringutil.String(fi.Size))
	values.Add(contract.FsHash, fi.Hash)
	values.Add(contract.FsCtime, stringutil.String(fi.CTime))
	values.Add(contract.FsAtime, stringutil.String(fi.ATime))
	values.Add(contract.FsMtime, stringutil.String(fi.MTime))
	if len(fi.HashValues) > 0 {
		values.Add(contract.FsHashValues, stringutil.String(fi.HashValues))
	}
	path := msg.BaseUrl + fsutil.SafePath(fi.Path) + fmt.Sprintf("?%s", values.Encode())

	switch action.Action(msg.Action) {
	case action.CreateAction:
		err = m.syncer.Create(path)
	case action.SymlinkAction:
		err = m.syncer.Symlink(fi.LinkTo, path)
	case action.WriteAction:
		err = m.syncer.Create(path)
		// ignore is not exist error
		if err != nil && os.IsNotExist(err) {
			err = nil
		}
		m.addWrite(path, fi.Size)
	case action.RemoveAction:
		m.removeWrite(path)
		err = m.syncer.Remove(path)
	case action.RenameAction:
		err = m.syncer.Rename(path)
	case action.ChmodAction:
		err = m.syncer.Chmod(path)
	}

	m.el.Write(eventlog.NewEvent(path, action.Action(msg.Action).String()))

	if err != nil {
		m.logger.Error(err, "%s action execute error => [%s]", action.Action(msg.Action).String(), path)
	}
	return err
}

// Close mark the monitor is closed, then close the connection
func (m *remoteClientMonitor) Close() error {
	m.closed.Store(true)
	if m.client != nil {
		return m.client.Stop()
	}
	return nil
}

//go:build linux || windows

package files

import (
	"encoding/json/jsontext"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/release"

	"github.com/fsnotify/fsnotify"
	"github.com/xtaci/smux"
)

func ServeAgent(conn io.ReadWriteCloser, version string, check ...func(MountApproval) error) error {
	return ServeAgentWithPeer(conn, version, "", nil, check...)
}

func ServeAgentWithPeer(conn io.ReadWriteCloser, version, account string, observe func(jsontext.Value) func(), check ...func(MountApproval) error) error {
	return serveMountAgent(conn, version, account, observe, "", check)
}

func ServeApprovedMount(conn io.ReadWriteCloser, version, account string, observe func(jsontext.Value) func(), approval MountApproval) error {
	session, err := smux.Server(conn, nil)
	if err != nil {
		_ = conn.Close()
		return err
	}
	data, err := session.AcceptStream()
	if err != nil {
		_ = session.Close()
		return err
	}
	check := []func(MountApproval) error{func(actual MountApproval) error {
		if actual != approval {
			return fmt.Errorf("mount approval identity changed")
		}
		return nil
	}}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		watch, err := session.AcceptStream()
		if err == nil {
			_ = serveMountAgent(watch, version, account, nil, "mount_watch", check)
		}
	}()
	err = serveMountAgent(data, version, account, observe, "mount_data", check)
	_ = session.Close()
	<-watchDone
	return err
}

func serveMountAgent(conn io.ReadWriteCloser, version, account string, observe func(jsontext.Value) func(), mode string, check []func(MountApproval) error) error {
	defer conn.Close()
	conn = newMountStream(conn)
	if os.Getenv("SSHORE_DISABLED") == "1" {
		return fmt.Errorf("file access disabled in recovery")
	}
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("mount agent requires amd64")
	}
	m, err := readMessage(conn)
	if err != nil {
		return err
	}
	err = release.Check(version, m.SoftwareVersion, "mount client")
	if err == nil && m.Type != "mount_data" && m.Type != "mount_watch" {
		err = fmt.Errorf("invalid mount handshake")
	}
	if err == nil && mode != "" && (m.Type != mode || m.Approval == nil) {
		err = fmt.Errorf("mount approval handshake changed")
	}
	if err == nil && m.Approval != nil {
		if len(check) != 1 {
			err = fmt.Errorf("mount approval validation unavailable")
		} else {
			err = check[0](*m.Approval)
		}
	}
	if err == nil && observe != nil && len(m.PeerMetadata) > 0 {
		if cleanup := observe(m.PeerMetadata); cleanup != nil {
			defer cleanup()
		}
	}
	reply := message{Type: "mount_ready", SoftwareVersion: version, Account: account}
	if err != nil {
		reply.Type = "error"
		reply.Error = err.Error()
	}
	if e := writeMessage(conn, reply); e != nil {
		return e
	}
	if err != nil {
		return err
	}
	if m.Type == "mount_watch" {
		serveFTPWatch(conn)
		return nil
	}
	return serveMountData(conn)
}

func serveFTPWatch(stream io.ReadWriteCloser) {
	defer stream.Close()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return
	} // stream EOF makes the client fall back to TTL
	stop := make(chan struct{})
	var workers sync.WaitGroup
	defer func() {
		close(stop)
		// Release a blocked read during endpoint shutdown.
		if s, ok := stream.(interface{ SetReadDeadline(time.Time) error }); ok {
			_ = s.SetReadDeadline(time.Now())
		}
		_ = stream.Close()
		watcher.Close()
		workers.Wait()
	}()
	requests := make(chan message, 1)
	workers.Go(func() {
		defer close(requests)
		for {
			m, e := readMessage(stream)
			if e != nil {
				return
			}
			select {
			case requests <- m:
			case <-stop:
				return
			}
		}
	})
	// The collector owns pending changes until the writer takes the whole batch.
	// Continue draining inotify while the writer is busy or cooling down.
	changed := make(chan watchChanges)
	workers.Go(func() {
		defer close(changed)
		var pending watchChanges
		var ready chan<- watchChanges
		for {
			select {
			case e, ok := <-watcher.Events:
				if !ok {
					return
				}
				reset := e.Has(fsnotify.Remove) || e.Has(fsnotify.Rename)
				name := mountWatchEvent(e.Name)
				pending.add(path.Dir(name), reset)
				if !pending.reset && slices.Contains(watcher.WatchList(), e.Name) {
					pending.add(name, false)
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
				pending.add("", true)
			case ready <- pending:
				pending = watchChanges{}
				ready = nil
				continue
			case <-stop:
				return
			}
			ready = changed
		}
	})
	timer := time.NewTimer(50 * time.Millisecond)
	timer.Stop()
	defer timer.Stop()
	var notifications <-chan watchChanges = changed
	send := func(m message) error {
		stop := time.AfterFunc(ftpWatchTimeout, func() { _ = stream.Close() })
		defer stop.Stop()
		return writeMessage(stream, m)
	}
	for {
		select {
		case m, ok := <-requests:
			if !ok {
				return
			}
			if m.Type != "watch" || !validFTPWatchPath(m.Directory) || (m.Name != "" && !validFTPWatchPath(m.Name)) {
				return
			}
			if m.Name != "" {
				if name, err := mountWatchPath(m.Name); err == nil {
					_ = watcher.Remove(name)
				}
			}
			ack := message{Type: "watch_ack", Error: "directory notifications unavailable"}
			if dir, err := mountWatchPath(m.Directory); err == nil {
				if info, err := os.Stat(dir); err == nil && info.IsDir() {
					watches := watcher.WatchList()
					if (len(watches) < ftpWatchLimit || slices.Contains(watches, dir)) && watcher.Add(dir) == nil {
						ack.Error = ""
					}
				}
			}
			if send(ack) != nil {
				return
			}
		case changes, ok := <-notifications:
			if !ok {
				return
			}
			if changes.reset {
				if send(message{Type: "watch_reset"}) != nil {
					return
				}
			} else {
				dirs := make([]string, 0, len(changes.dirs))
				dirs = slices.AppendSeq(dirs, maps.Keys(changes.dirs))
				slices.Sort(dirs)
				for _, dir := range dirs {
					if send(message{Type: "watch_dirty", Directory: dir}) != nil {
						return
					}
				}
			}
			notifications = nil
			timer.Reset(50 * time.Millisecond)
		case <-timer.C:
			notifications = changed
		}
	}
}

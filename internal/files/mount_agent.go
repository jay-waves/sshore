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

	"github.com/fsnotify/fsnotify"
	"github.com/xtaci/smux"
)

func ServeAgent(conn io.ReadWriteCloser, mode, version, account, home string, peer jsontext.Value, observe func(jsontext.Value) func()) error {
	defer conn.Close()
	return serveMountAgent(newMountStream(conn), message{Type: mode, PeerMetadata: peer}, version, account, home, observe, nil)
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
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		watch, err := session.AcceptStream()
		if err == nil {
			_ = serveApprovedMountStream(watch, version, account, nil, "mount_watch", approval)
		}
	}()
	err = serveApprovedMountStream(data, version, account, observe, "mount_data", approval)
	_ = session.Close()
	<-watchDone
	return err
}

func serveApprovedMountStream(conn io.ReadWriteCloser, version, account string, observe func(jsontext.Value) func(), mode string, approval MountApproval) error {
	defer conn.Close()
	wire := newMountStream(conn)
	m, err := readMessage(wire)
	if err != nil {
		return err
	}
	if m.SoftwareVersion != version {
		return writeMessage(wire, message{Type: "error", SoftwareVersion: version,
			Error: fmt.Sprintf("mount version mismatch: agent=%s, client=%q", version, m.SoftwareVersion)})
	}
	if m.Type != mode || m.Approval == nil {
		return writeMessage(wire, message{Type: "error", SoftwareVersion: version, Error: "mount approval handshake changed"})
	}
	return serveMountAgent(wire, m, version, account, "", observe, &approval)
}

func serveMountAgent(conn io.ReadWriteCloser, m message, version, account, home string, observe func(jsontext.Value) func(), approval *MountApproval) error {
	if os.Getenv("SSHORE_DISABLED") == "1" {
		return fmt.Errorf("file access disabled in recovery")
	}
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("mount agent requires amd64")
	}
	var err error
	if m.Type != "mount_data" && m.Type != "mount_watch" {
		err = fmt.Errorf("invalid mount handshake")
	}
	if err == nil && m.Approval != nil {
		if approval == nil {
			err = fmt.Errorf("mount approval validation unavailable")
		} else if *m.Approval != *approval {
			err = fmt.Errorf("mount approval identity changed")
		}
	}
	if err == nil && observe != nil && len(m.PeerMetadata) > 0 {
		if cleanup := observe(m.PeerMetadata); cleanup != nil {
			defer cleanup()
		}
	}
	reply := message{Type: "mount_ready", SoftwareVersion: version}
	if m.Type == "mount_data" {
		reply.Account, reply.HomeDirectory = account, home
	}
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

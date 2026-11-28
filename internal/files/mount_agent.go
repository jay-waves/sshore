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
	"github.com/jay-waves/sshore/internal/ssh"
)

// ServeMount serves the same stream protocol on a direct exec channel or an
// approved resource route. The data handshake authorizes the entire carrier.
func ServeMount(conn io.ReadWriteCloser, version, account, home string, approval *MountApproval, observe func(jsontext.Value) func()) error {
	defer conn.Close()
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("mount agent requires amd64")
	}
	session, err := ssh.NewStreamMux(conn, false)
	if err != nil {
		return err
	}
	defer session.Close()
	stream, err := session.AcceptStream()
	if err != nil {
		return err
	}
	data := newMountStream(stream)
	request, err := readMessage(data)
	if err != nil {
		return err
	}
	if request.SoftwareVersion != version {
		err = fmt.Errorf("mount version mismatch: agent=%s, client=%q", version, request.SoftwareVersion)
	} else if request.Type != "mount" {
		err = fmt.Errorf("invalid mount handshake")
	} else if (request.Approval == nil) != (approval == nil) || approval != nil && *request.Approval != *approval {
		err = fmt.Errorf("mount approval identity changed")
	}
	if err != nil {
		_ = writeMessage(data, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
		return err
	}
	if observe != nil && len(request.PeerMetadata) > 0 {
		if cleanup := observe(request.PeerMetadata); cleanup != nil {
			defer cleanup()
		}
	}
	if err := writeMessage(data, message{Type: "mount_ready", SoftwareVersion: version, Account: account, HomeDirectory: home}); err != nil {
		return err
	}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		if watch, err := session.AcceptStream(); err == nil {
			defer watch.Close()
			serveFTPWatch(newMountStream(watch))
		}
	}()
	err = serveMountData(data)
	_ = session.Close()
	<-watchDone
	return err
}

func serveFTPWatch(stream *mountStream) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return
	} // stream EOF makes the client fall back to TTL
	stop := make(chan struct{})
	var workers sync.WaitGroup
	defer func() {
		close(stop)
		// Release a blocked read during endpoint shutdown.
		if s, ok := stream.ReadWriteCloser.(interface{ SetReadDeadline(time.Time) error }); ok {
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

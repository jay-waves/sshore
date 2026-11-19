//go:build linux

package files

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/xtaci/smux"
)

func ftpSFTPServerExecutable() (string, error) {
	for _, name := range []string{"/usr/libexec/openssh/sftp-server", "/usr/lib/openssh/sftp-server", "/usr/lib/ssh/sftp-server", "/usr/libexec/sftp-server", "sftp-server"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("OpenSSH sftp-server executable unavailable; install the OpenSSH server component")
}

func ServeAgent(conn io.ReadWriteCloser, version string, check ...func(MountApproval) error) error {
	return ServeAgentWithPeer(conn, version, "", nil, check...)
}

func ServeAgentWithPeer(conn io.ReadWriteCloser, version, account string, observe func(json.RawMessage) func(), check ...func(MountApproval) error) error {
	defer conn.Close()
	if os.Getenv("SSHORE_DISABLED") == "1" {
		return fmt.Errorf("file access disabled in recovery")
	}
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("mount agent requires Linux amd64")
	}
	m, err := readMessage(conn)
	if err != nil {
		return err
	}
	err = checkPeerVersion(version, m.SoftwareVersion, "mount client")
	if err == nil && m.Type != "mount_mux" {
		err = fmt.Errorf("invalid mount handshake")
	}
	if err == nil && m.Approval != nil {
		if len(check) != 1 {
			err = fmt.Errorf("mount approval validation unavailable")
		} else {
			err = check[0](*m.Approval)
		}
	}
	executable := ""
	if err == nil {
		executable, err = ftpSFTPServerExecutable()
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
	mux, err := smux.Server(conn, muxConfig())
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	defer func() {
		mux.Close()
		workers.Wait()
	}()
	data, err := mux.AcceptStream()
	if err != nil {
		return err
	}
	events, err := mux.AcceptStream()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, executable)
	// Own the copying goroutines: os/exec.Wait must not depend on a stream Read
	// that can remain blocked after the child has exited.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, childOutput, err := os.Pipe()
	if err != nil {
		return err
	}
	defer stdout.Close()
	cmd.Stdout, cmd.Stderr = childOutput, os.Stderr
	cmd.Env = append(os.Environ(), "SSHORE_SOCKET=", "SSHORE_CONFIRM_ADDR=", "SSHORE_CONFIRM_TOKEN=")
	err = cmd.Start()
	childOutput.Close()
	if err != nil {
		return err
	}
	workers.Go(func() {
		select {
		case <-mux.CloseChan():
			cancel()
		case <-ctx.Done():
		}
	})
	outputDone := make(chan struct{})
	workers.Go(func() {
		defer stdin.Close()
		_, _ = io.Copy(stdin, data)
	})
	workers.Go(func() {
		defer close(outputDone)
		if _, err := io.Copy(data, stdout); err != nil {
			cancel()
			return
		}
		_ = data.CloseWrite()
	})
	workers.Go(func() { serveFTPWatch(events) })
	err = cmd.Wait()
	// Preserve final replies while bounding cleanup for a peer that stopped reading.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-outputDone:
	case <-mux.CloseChan():
	case <-timer.C:
	}
	return err
}

func serveFTPWatch(stream *smux.Stream) {
	defer stream.Close()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return
	} // stream EOF makes the client fall back to TTL
	stop := make(chan struct{})
	var workers sync.WaitGroup
	defer func() {
		close(stop)
		// Release a blocked read before joining, independently of the stream FIN.
		_ = stream.SetReadDeadline(time.Now())
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
				pending.add(path.Dir(e.Name), reset)
				if !pending.reset && slices.Contains(watcher.WatchList(), e.Name) {
					pending.add(e.Name, false)
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
		_ = stream.SetWriteDeadline(time.Now().Add(ftpWatchTimeout))
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
				_ = watcher.Remove(m.Name)
			}
			ack := message{Type: "watch_ack", Error: "directory notifications unavailable"}
			if info, err := os.Stat(m.Directory); err == nil && info.IsDir() {
				watches := watcher.WatchList()
				if (len(watches) < ftpWatchLimit || slices.Contains(watches, m.Directory)) && watcher.Add(m.Directory) == nil {
					ack.Error = ""
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

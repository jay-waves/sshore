//go:build linux

package files

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"runtime"
	"slices"
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
	return "", fmt.Errorf("OpenSSH sftp-server executable unavailable; install the server component or use --no-watch")
}

func ServeAgent(conn io.ReadWriteCloser, version string) error {
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
	executable := ""
	if err == nil {
		executable, err = ftpSFTPServerExecutable()
	}
	reply := message{Type: "mount_ready", SoftwareVersion: version}
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
	defer mux.Close()
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
	defer childOutput.Close()
	cmd.Stdout, cmd.Stderr = childOutput, os.Stderr
	cmd.Env = append(os.Environ(), "SSHORE_SOCKET=", "SSHORE_ASKPASS=", "SSHORE_ASKPASS_ADDR=", "SSHORE_ASKPASS_TOKEN=")
	if err = cmd.Start(); err != nil {
		return err
	}
	childOutput.Close()
	inputDone, outputDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(inputDone); _, _ = io.Copy(stdin, data); stdin.Close() }()
	go func() { defer close(outputDone); _, _ = io.Copy(data, stdout); data.Close() }()
	defer func() { data.Close(); stdin.Close(); stdout.Close(); <-inputDone; <-outputDone }()

	watchDone := make(chan struct{})
	go func() { defer close(watchDone); serveFTPWatch(events) }()
	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()
	defer func() { mux.Close(); events.Close(); <-watchDone }()
	select {
	case err = <-childDone:
		// Preserve final replies while bounding cleanup for a peer that stopped reading.
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-outputDone:
		case <-mux.CloseChan():
		case <-timer.C:
		}
		return err
	case <-mux.CloseChan():
		cancel()
		data.Close() // release the stdin copier before waiting
		return <-childDone
	}
}

func serveFTPWatch(stream *smux.Stream) {
	defer stream.Close()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return
	} // stream EOF makes the client fall back to TTL
	defer watcher.Close()
	stop := make(chan struct{})
	defer close(stop)
	requests := make(chan message, 1)
	go func() {
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
	}()
	// Drain inotify even while the network writer is blocked. A one-slot signal
	// coalesces bursts; overflow loses detail but always invalidates the cache.
	changed := make(chan struct{}, 1)
	var changes watchChanges
	go func() {
		for {
			select {
			case e, ok := <-watcher.Events:
				if !ok {
					return
				}
				reset := e.Has(fsnotify.Remove) || e.Has(fsnotify.Rename)
				changes.add(path.Dir(e.Name), reset)
				if !reset && slices.Contains(watcher.WatchList(), e.Name) {
					changes.add(e.Name, false)
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
				changes.add("", true)
			case <-stop:
				return
			}
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}()
	var cooldown <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	var notifications <-chan struct{} = changed
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
			var e error
			info, statErr := os.Stat(m.Directory)
			if statErr != nil {
				e = statErr
			} else if !info.IsDir() {
				e = fmt.Errorf("watch target is not a directory")
			}
			if e == nil {
				watches := watcher.WatchList()
				if len(watches) >= ftpWatchLimit && !slices.Contains(watches, m.Directory) {
					e = fmt.Errorf("watch limit reached")
				} else {
					e = watcher.Add(m.Directory)
				}
			}
			ack := message{Type: "watch_ack"}
			if e != nil {
				ack.Error = "directory notifications unavailable"
			}
			if send(ack) != nil {
				return
			}
		case <-notifications:

			reset, dirs := changes.take()
			if reset {
				if send(message{Type: "watch_reset"}) != nil {
					return
				}
			} else {
				for _, dir := range dirs {
					if send(message{Type: "watch_dirty", Directory: dir}) != nil {
						return
					}
				}
			}
			notifications = nil
			if timer == nil {
				timer = time.NewTimer(50 * time.Millisecond)
			} else {
				timer.Reset(50 * time.Millisecond)
			}
			cooldown = timer.C
		case <-cooldown:
			notifications, cooldown = changed, nil
		}
	}
}

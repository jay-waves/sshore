package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"github.com/xtaci/smux"

	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/transport"
)

// MountConfig contains validated mount and SSH transport settings.
type MountConfig struct {
	Approval                                         *MountApproval
	Host, Remote, Mountpoint, SSHExecutable, Version string
	SSHArgs                                          []string
	ReadOnly, DirectIO, NoWatch, Stats               bool
	StartupTimeout, IOTimeout                        time.Duration
	Prepare                                          func(context.Context, io.ReadWriter) error
}

// MountLifecycle connects a background worker to the mount. Ready and Sync are
// required when supplied; Sync returns a cleanup function. Nil means foreground.
type MountLifecycle struct {
	Abandoned          <-chan struct{}
	Environment        []string
	OperationCompleted func()
	Ready              func(string, string) error
	Sync               func(context.Context, *sftp.Client) func()
	// Stop receives a reply channel for the graceful-unmount result.
	Stop <-chan chan error
}
type ftpMount interface {
	Unmount() error
	Wait()
}

func RunMount(o MountConfig, service *MountLifecycle) (code int, resultErr error) {
	var err error
	if err = CheckMountPlatform(); err != nil {
		return 1, err
	}
	if err = ValidateMountpoint(o.Mountpoint); err != nil {
		return 1, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupts)
	var abandoned <-chan struct{}
	if service != nil {
		abandoned = service.Abandoned
	}
	setupFinished, setupStopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(setupStopped)
		select {
		case <-abandoned:
			cancel()
		case <-interrupts:
			cancel()
		case <-setupFinished:
		case <-ctx.Done():
		}
	}()
	defer func() { cancel(); <-setupStopped }()
	exe := o.SSHExecutable
	cmd := exec.CommandContext(ctx, exe, o.SSHArgs...)
	transport.IsolateCommand(cmd, service != nil)
	cmd.Stderr = os.Stderr
	if service != nil {
		cmd.Env = service.Environment
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}
	if err = cmd.Start(); err != nil {
		return 1, err
	}
	defer func() {
		_ = stdin.Close()
		_ = stdout.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	watch := &sftpWatch{}
	abortIO := func() { cancel(); _ = stdout.Close(); _ = stdin.Close() }
	defer func() {
		if failure := watch.err(); failure != nil {
			code = 1
			resultErr = errors.Join(resultErr, failure)
		}
	}()
	if o.StartupTimeout > 0 {
		watch.timer = time.AfterFunc(o.StartupTimeout, func() {
			watch.fail(fmt.Errorf("mount startup timed out after %s (including SSH authentication)", o.StartupTimeout), abortIO)
		})
		defer watch.timer.Stop()
	}
	var events *smux.Stream
	wire := newFTPWire(stdout, stdin)
	if !o.NoWatch {
		transport := &transport.Pipe{Reader: stdout, Writer: stdin, Closers: []io.Closer{stdout, stdin}}
		if o.Prepare != nil {
			if err := o.Prepare(ctx, transport); err != nil {
				return 1, fmt.Errorf("mount setup: %w", err)
			}
		}
		mux, data, notifications, err := OpenMux(transport, o.Version, o.Approval)
		if err != nil {
			return 1, fmt.Errorf("mount agent handshake: %w", err)
		}
		defer mux.Close()
		events = notifications
		wire = newFTPWire(data, data)
	}
	wire.metrics = o.Stats
	if service != nil {
		wire.onReply = service.OperationCompleted
	}
	stopWatch := wire.watchRequests(o.IOTimeout, watch, abortIO)
	defer stopWatch()
	client, err := sftp.NewClientPipe(wire, wire)
	if err != nil {
		return 1, fmt.Errorf("SFTP handshake: %w", err)
	}
	// Direct pipe closure must precede Client.Close: pkg/sftp serializes Close
	// with writes and otherwise cannot interrupt a blocked outgoing request.
	defer func() { cancel(); _ = stdout.Close(); _ = stdin.Close(); _ = client.Close() }()
	root, err := client.RealPath(o.Remote)
	if err != nil {
		return 1, fmt.Errorf("remote directory: %w", err)
	}
	info, err := client.Stat(root)
	if err != nil {
		return 1, err
	}
	if !info.IsDir() {
		return 1, fmt.Errorf("remote path is not a directory")
	}
	if !o.ReadOnly {
		for _, ext := range []string{"posix-rename@openssh.com", "fsync@openssh.com"} {
			if _, ok := client.HasExtension(ext); !ok {
				return 1, fmt.Errorf("writable mounts require SFTP extension %s; use --read-only or update the server", ext)
			}
		}
	}
	backend := &ftpFS{client: client, root: path.Clean(root), readOnly: o.ReadOnly, directIO: o.DirectIO}
	if events != nil {
		backend.notifications = startFTPNotifications(events, &backend.cache)
		defer backend.notifications.Close()
		backend.watchDirectory(backend.root)
	}
	if o.Stats {
		backend.metrics = &ftpCallbackMetrics{}
		stopStats := startFTPStats(wire, backend, os.Stderr)
		defer stopStats()
	}
	backend.prefetch = startDirectoryPrefetch(ctx, backend)
	defer func() {
		backend.prefetch.cancel()
		// Release an outstanding SFTP CLOSE before waiting for the worker.
		abortIO()
		<-backend.prefetch.done
	}()
	mount, err := mountFTP(backend, o.Mountpoint)
	if err != nil {
		return 1, err
	}
	if watch.timer != nil {
		watch.timer.Stop()
	}
	if err := watch.err(); err != nil {
		_ = mount.Unmount()
		return 1, err
	}
	close(setupFinished)
	<-setupStopped
	if service != nil {
		if err = service.Ready(root, o.Mountpoint); err != nil {
			cancel()
			_ = stdout.Close()
			_ = stdin.Close()
			_ = mount.Unmount()
			return 1, err
		}
	}
	if service != nil {
		stopSync := service.Sync(ctx, client)
		defer stopSync()
	}
	fmt.Fprintf(os.Stderr, "Mounted %s:%s -> %s (SFTP). Close files, then Ctrl-C to unmount.\n", termui.CleanText(o.Host), termui.CleanText(root), termui.CleanText(o.Mountpoint))
	done := make(chan struct{})
	go func() { mount.Wait(); close(done) }()
	lost := make(chan error, 1)
	go func() { lost <- client.Wait() }()
	abort := func() { cancel(); _ = stdout.Close(); _ = stdin.Close() }
	var requests <-chan chan error
	if service != nil {
		requests = service.Stop
	}
	for {
		select {
		case request := <-requests:
			stopped, stopErr := stopFTPMount(mount, interrupts, abort, 10*time.Second)
			request <- stopErr
			if stopped {
				if stopErr != nil {
					return 1, stopErr
				}
				return 0, nil
			}
		case <-done:
			return 0, nil
		case <-ctx.Done():
			abort()
			_ = mount.Unmount()
			return 1, fmt.Errorf("mount connection interrupted during setup")
		case <-interrupts:
			fmt.Fprintln(os.Stderr, "Unmounting (up to 10s). Ctrl-C again forces disconnect.")
			stopped, stopErr := stopFTPMount(mount, interrupts, abort, 10*time.Second)
			if !stopped {
				fmt.Fprintf(os.Stderr, "Unmount failed: %s. Still connected; close files and retry Ctrl-C.\n", termui.CleanText(stopErr.Error()))
				continue
			}
			if stopErr != nil {
				return 1, stopErr
			}
			return 0, nil
		case err = <-lost:
			abort()
			_, unmountErr := stopFTPMount(mount, interrupts, abort, 5*time.Second)
			if unmountErr != nil {
				return 1, fmt.Errorf("SFTP disconnected (%v); unmount: %w", err, unmountErr)
			}
			return 1, fmt.Errorf("SFTP disconnected: %v", err)
		}
	}
}

// stopFTPMount leaves a busy mount connected after a normal unmount refusal.
// It only interrupts transport after the grace period or an explicit second
// signal. Waiting is bounded even if a platform's unmount cannot be interrupted.
func stopFTPMount(m ftpMount, interrupts <-chan os.Signal, abort func(), grace time.Duration) (bool, error) {
	result := make(chan error, 1)
	go func() { result <- m.Unmount() }()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	reason := "unmount timed out"
	select {
	case e := <-result:
		if e != nil {
			return false, e
		}
		return true, nil
	case <-interrupts:
		reason = "forced disconnect"
	case <-timer.C:
	}
	abort()
	timer.Reset(5 * time.Second)
	select {
	case e := <-result:
		if e != nil {
			return true, fmt.Errorf("%s; unmount failed: %w", reason, e)
		}
	case <-timer.C:
		return true, fmt.Errorf("%s; unmount still pending; in-flight writes may be incomplete", reason)
	}
	return true, fmt.Errorf("%s; in-flight writes may be incomplete", reason)
}

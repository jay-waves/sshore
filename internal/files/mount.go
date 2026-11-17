package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"sync"
	"syscall"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/pkg/sftp"
	"github.com/xtaci/smux"
)

// MountConfig contains validated mount and SSH transport settings.
type MountConfig struct {
	PeerMetadata                      json.RawMessage
	Approval                          *MountApproval
	Host, Remote, Mountpoint, Version string
	Connection                        sshio.Options
	ReadOnly, DirectIO, Stats         bool
	StartupTimeout, IOTimeout         time.Duration
	Prepare                           func(context.Context, io.ReadWriter) error
	Open                              func(context.Context) (io.ReadWriteCloser, error)
}

// MountLifecycle connects a background worker to the mount. Nil means foreground.
type MountLifecycle struct {
	Abandoned          <-chan struct{}
	Environment        []string
	OperationCompleted func()
	Ready              func(string, string) error
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
	connection := o.Connection
	if service != nil {
		connection.Environment = service.Environment
	}
	var transport io.ReadWriteCloser
	if o.Open != nil {
		transport, err = o.Open(ctx)
	} else {
		var pipes *sshio.Process
		pipes, err = sshio.Start(ctx, connection, os.Stderr)
		if err == nil {
			transport = pipes
			defer func() { pipes.Close(); pipes.Wait() }()
		}
	}
	if err != nil {
		return 1, err
	}
	defer transport.Close()
	stdout, stdin := transport, transport
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
	if o.Prepare != nil {
		if err := o.Prepare(ctx, transport); err != nil {
			return 1, fmt.Errorf("mount setup: %w", err)
		}
	}
	mux, data, events, err := openMux(transport, o.Version, o.Approval, o.PeerMetadata)
	if err != nil {
		return 1, fmt.Errorf("mount agent handshake: %w", err)
	}
	defer mux.Close()
	wire := newFTPWire(data, data)
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
	backend.notifications = startFTPNotifications(events, &backend.cache)
	defer backend.notifications.Close()
	backend.watchDirectory(backend.root)
	if o.Stats {
		backend.metrics = &ftpCallbackMetrics{}
		stopStats := startFTPStats(wire, backend, os.Stderr)
		defer stopStats()
	}
	refresh := startMetadataRefresh(ctx, backend)
	backend.prefetch = startDirectoryPrefetch(ctx, backend)
	defer func() {
		backend.prefetch.cancel()
		refresh.cancel()
		// Release an outstanding SFTP CLOSE before waiting for the worker.
		abortIO()
		<-backend.prefetch.done
		<-refresh.done
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

type MountApproval struct {
	Instance string `json:",omitempty"`
	Session  string
	PID      int
	ID       string
}

type message struct {
	Approval     *MountApproval  `json:"mount_approval,omitempty"`
	PeerMetadata json.RawMessage `json:"peer_metadata,omitempty"`

	Type            string `json:"type"`
	SoftwareVersion string `json:"software_version,omitempty"`
	Error           string `json:"error,omitempty"`
	Directory       string `json:"directory,omitempty"`
	Name            string `json:"name,omitempty"`
	Data            []byte `json:"-"`
}

func (m *message) FrameType() string                 { return m.Type }
func (m *message) SetFrame(kind string, data []byte) { m.Type, m.Data = kind, data }
func writeMessage(w io.Writer, m message) error {
	return sshio.WriteMessage(w, m.Type, m.Data, m)
}
func readMessage(r io.Reader) (message, error) {
	var m message
	err := sshio.ReadMessage(r, &m)
	return m, err
}

func checkPeerVersion(local, peer, role string) error {
	if local != "" && local == peer {
		return nil
	}
	return fmt.Errorf("sshore version mismatch: this process=%s, %s=%s; reconnect to update or recover", local, role, peer)
}
func muxConfig() *smux.Config {
	config := sshio.MuxConfig()
	// Mounts have separate data/notification streams and no terminal to compete
	// with. A larger data window avoids throttling pipelined SFTP on higher RTTs.
	config.MaxStreamBuffer = 4 * 1024 * 1024
	// Only data and notifications share this connection; reserve headroom for both.
	config.MaxReceiveBuffer = 3 * config.MaxStreamBuffer
	return config
}

type ftpRequestTotals struct {
	Started, Completed, Errors uint64
	SentBytes, ReceivedBytes   uint64
	TotalNS, MaxNS             int64
}
type ftpCallbackTotals struct {
	Calls          uint64
	TotalNS, MaxNS int64
}
type ftpCallbackMetrics struct {
	mu     sync.Mutex
	totals map[string]ftpCallbackTotals
}

func (m *ftpCallbackMetrics) observe(name string) func() {
	if m == nil {
		return func() {}
	}
	start := time.Now()
	return func() {
		elapsed := int64(time.Since(start))
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.totals == nil {
			m.totals = make(map[string]ftpCallbackTotals)
		}
		v := m.totals[name]
		v.Calls++
		v.TotalNS += elapsed
		v.MaxNS = max(v.MaxNS, elapsed)
		m.totals[name] = v
	}
}
func writeFTPStats(w *ftpWire, f *ftpFS, out io.Writer) {
	report := struct {
		Type                                                       string
		Requests                                                   map[string]ftpRequestTotals
		Inflight, PeakInflight                                     int
		Cache                                                      ftpCacheCounters
		CacheBytes, MetadataEntries, Directories, DirectoryEntries int
		Callbacks                                                  map[string]ftpCallbackTotals
	}{Type: "mount_stats", Requests: make(map[string]ftpRequestTotals), Callbacks: make(map[string]ftpCallbackTotals)}
	w.mu.Lock()
	for k, v := range w.totals {
		if v.Started != 0 {
			report.Requests[ftpRequestName(byte(k))] = v
		}
	}
	report.Inflight, report.PeakInflight = len(w.requests), w.peakRequests
	w.mu.Unlock()
	f.cache.mu.Lock()
	report.Cache = f.cache.counters
	report.CacheBytes, report.MetadataEntries, report.Directories, report.DirectoryEntries = f.cache.bytes, f.cache.stats, f.cache.dirs, f.cache.dirEntries
	f.cache.mu.Unlock()
	if f.metrics != nil {
		f.metrics.mu.Lock()
		for k, v := range f.metrics.totals {
			report.Callbacks[k] = v
		}
		f.metrics.mu.Unlock()
	}
	_ = json.NewEncoder(out).Encode(report)
}
func startFTPStats(w *ftpWire, f *ftpFS, out io.Writer) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				writeFTPStats(w, f, out)
				return
			case <-ticker.C:
				writeFTPStats(w, f, out)
			}
		}
	}()
	return func() { close(stop); <-done }
}

func ftpRequestName(kind byte) string {
	names := map[byte]string{3: "OPEN", 4: "CLOSE", 5: "READ", 6: "WRITE", 7: "LSTAT", 8: "FSTAT", 9: "SETSTAT", 10: "FSETSTAT", 11: "OPENDIR", 12: "READDIR", 13: "REMOVE", 14: "MKDIR", 15: "RMDIR", 16: "REALPATH", 17: "STAT", 18: "RENAME", 19: "READLINK", 20: "SYMLINK", 200: "EXTENDED"}
	if name, ok := names[kind]; ok {
		return name
	}
	return fmt.Sprintf("TYPE_%d", kind)
}

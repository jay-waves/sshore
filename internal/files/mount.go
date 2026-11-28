package files

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"path"
	"sync"
	"syscall"
	"time"

	sftp "github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
	"github.com/pkg/sftp/v2/encoding/ssh/filexfer/openssh"
	"github.com/pkg/sftp/v2/localfs"
	"github.com/xtaci/smux"
)

// MountConfig contains mount settings and endpoint openers. Open provides the
// data channel for direct SSH or the transport for an approved mount.
type MountConfig struct {
	History                     func(account, root string, delta VisitHistory) VisitHistory
	PeerMetadata                jsontext.Value
	Approval                    *MountApproval
	Remote, Mountpoint, Version string
	Name                        string
	ReadOnly, DirectIO          bool
	StartupTimeout, IOTimeout   time.Duration
	Open                        func(context.Context) (io.ReadWriteCloser, error)
	OpenNotifications           func(context.Context) (io.ReadWriteCloser, error)
	SelectPaths                 func(context.Context, DirectoryReader, string) (string, string, error)
	NotificationsUnavailable    func()
	ReportStats                 func(MountStats)
}

// MountLifecycle connects the worker's control pipe to the mount.
type MountLifecycle struct {
	OwnerDone <-chan struct{}
	Ready     func(string, string) error
	// Stop receives a callback for the graceful-unmount result.
	Stop <-chan func(error)
}
type ftpMount interface {
	Unmount() error
	Wait()
}

func RunMount(o MountConfig, service MountLifecycle) (code int, resultErr error) {
	var stoppedReply func(error)
	defer func() {
		if stoppedReply != nil {
			stoppedReply(resultErr)
		}
	}()
	var err error
	if err = CheckMountPlatform(); err != nil {
		return 1, err
	}
	cleanup := func() {}
	var automaticMountpoint bool
	defer func() { cleanup() }()
	prepareMountpoint := func() error {
		automaticMountpoint = o.Mountpoint == ""
		point, release, err := PrepareMountpoint(o.Mountpoint, o.Name)
		if err != nil {
			return err
		}
		o.Mountpoint, cleanup = point, release
		if automaticMountpoint {
			return nil
		}
		return ValidateMountpoint(point)
	}
	if o.SelectPaths == nil {
		if err := prepareMountpoint(); err != nil {
			return 1, err
		}
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	defer func() {
		if cause := context.Cause(ctx); cause != nil && cause != context.Canceled && !errors.Is(resultErr, cause) {
			code = 1
			resultErr = errors.Join(resultErr, cause)
		}
	}()
	signals, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stopSignals()
	var startupTimer *time.Timer
	var startupDeadline <-chan time.Time
	if o.StartupTimeout > 0 {
		startupTimer = time.NewTimer(o.StartupTimeout)
		startupDeadline = startupTimer.C
	}
	setup, endSetup := context.WithCancel(ctx)
	setupStopped := make(chan struct{})
	go func() {
		defer close(setupStopped)
		if startupTimer != nil {
			defer startupTimer.Stop()
		}
		select {
		case <-service.OwnerDone:
			cancel(errors.New("mount startup abandoned"))
		case <-signals.Done():
			cancel(errors.New("mount startup interrupted"))
		case <-startupDeadline:
			cancel(fmt.Errorf("mount startup timed out after %s (including SSH authentication)", o.StartupTimeout))
		case <-setup.Done():
		}
	}()
	finishSetup := func() { endSetup(); <-setupStopped }
	defer finishSetup()
	endpoint, err := o.Open(ctx)
	if err != nil {
		return 1, err
	}
	closeEndpoint := endpoint.Close
	var session *smux.Session
	if o.Approval != nil {
		session, err = smux.Client(endpoint, nil)
		if err != nil {
			_ = endpoint.Close()
			return 1, err
		}
		closeEndpoint = session.Close
		o.OpenNotifications = func(context.Context) (io.ReadWriteCloser, error) {
			return session.OpenStream()
		}
	}
	closeTransport := sync.OnceFunc(func() { _ = closeEndpoint() })
	stopTransport := context.AfterFunc(ctx, closeTransport)
	defer stopTransport()
	disconnect := func(reason error) {
		cancel(reason)
		closeTransport()
	}
	if session != nil {
		endpoint, err = session.OpenStream()
		if err != nil {
			disconnect(nil)
			return 1, err
		}
	}
	data := newMountStream(endpoint)
	var account, root string
	var client *sftp.Client
	var backend *ftpFS
	var stopStats func()
	var sftpWorkers sync.WaitGroup
	defer func() {
		// Cut transport before joining any worker with outstanding SFTP I/O.
		// Client.Close itself can wait behind a blocked write.
		disconnect(nil)
		if backend != nil && backend.prefetch != nil {
			<-backend.prefetch.done
			<-backend.cache.refresh.done
			if o.History != nil && account != "" {
				backend.prefetch.mu.Lock()
				delta := backend.prefetch.delta
				backend.prefetch.delta = nil
				backend.prefetch.mu.Unlock()
				if len(delta) > 0 {
					o.History(account, root, delta)
				}
			}
			if stopStats != nil {
				stopStats()
			}
			backend.notifications.Close()
		}
		if client != nil {
			_ = client.Close()
		}
		sftpWorkers.Wait()
	}()
	ready, err := negotiateMount(data, "mount_data", o.Version, o.Approval, o.PeerMetadata)
	if err != nil {
		return 1, fmt.Errorf("mount agent handshake: %w", err)
	}
	account = ready.Account
	var events io.ReadWriteCloser
	if o.OpenNotifications != nil {
		endpoint, watchErr := o.OpenNotifications(ctx)
		if watchErr == nil {
			watch := newMountStream(endpoint)
			closeWatch := sync.OnceFunc(func() { _ = watch.Close() })
			stopWatch := context.AfterFunc(ctx, closeWatch)
			defer func() { stopWatch(); closeWatch() }()
			_, watchErr = negotiateMount(watch, "mount_watch", o.Version, o.Approval, nil)
			if watchErr == nil {
				events = watch
			} else {
				closeWatch()
			}
		}
	}
	ftp := newFTPWire(data, data)
	if o.ReportStats != nil {
		ftp.enableMetrics()
	}
	if o.IOTimeout > 0 {
		sftpWorkers.Go(func() {
			if err := ftp.watchRequests(ctx, o.IOTimeout); err != nil {
				cancel(err)
			}
		})
	}
	// The same limit covers directory replies on every client and 128 KiB FUSE reads in two requests.
	client, err = newMountSFTPClient(ctx, ftp, o.IOTimeout)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return 1, cause
		}
		return 1, fmt.Errorf("SFTP handshake: %w", err)
	}
	sftpWorkers.Go(func() {
		err := client.Wait()
		if err == nil {
			err = io.EOF
		}
		cancel(fmt.Errorf("SFTP disconnected: %w", err))
	})
	backend = &ftpFS{client: client, readOnly: o.ReadOnly, directIO: o.DirectIO}
	if o.SelectPaths != nil {
		o.Remote, o.Mountpoint, err = o.SelectPaths(ctx, backend.browseDirectory, ready.HomeDirectory)
		if err != nil {
			return 1, err
		}
		if err := prepareMountpoint(); err != nil {
			return 1, err
		}
	}
	remote, err := ExpandRemoteHome(o.Remote, ready.HomeDirectory)
	if err != nil {
		return 1, err
	}
	root, err = client.RealPath(remote)
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
		for _, extension := range []*sshfx.ExtensionPair{openssh.ExtensionPOSIXRename(), openssh.ExtensionFSync()} {
			if !client.HasExtension(extension) {
				return 1, fmt.Errorf("writable mounts require SFTP extension %s; use --read-only or update the server", extension.Name)
			}
		}
	}
	var history VisitHistory
	if o.History != nil && account != "" {
		history = o.History(account, root, nil)
	}
	backend.root = path.Clean(root)
	backend.notifications = startFTPNotifications(events, &backend.cache, o.NotificationsUnavailable)
	backend.notifications.enqueue(backend.root)
	if o.ReportStats != nil {
		backend.metrics = &ftpCallbackMetrics{}
		stopStats = startFTPStats(ftp, backend, o.ReportStats)
	}
	startMetadataRefresh(ctx, backend)
	backend.prefetch = startDirectoryPrefetch(ctx, backend, history)
	mount, point, err := mountFTP(backend, o.Mountpoint, automaticMountpoint)
	if err != nil {
		return 1, err
	}
	o.Mountpoint = point
	finishSetup()
	if ctx.Err() == nil {
		if err := service.Ready(root, o.Mountpoint); err != nil {
			cancel(err)
		}
	}
	var done <-chan struct{}
	if ctx.Err() == nil {
		mounted := make(chan struct{})
		go func() { mount.Wait(); close(mounted) }()
		done = mounted
	}
	const unmountGrace = 10 * time.Second
	for {
		var reply func(error)
		if ctx.Err() == nil {
			select {
			case reply = <-service.Stop:
			case <-done:
				return 0, nil
			case <-ctx.Done():
			case <-service.OwnerDone:
			case <-signals.Done():
			}
		}
		stopped, stopErr := stopFTPMount(ctx, mount, disconnect, unmountGrace)
		if stopped {
			stoppedReply = reply
			if stopErr != nil {
				return 1, stopErr
			}
			return 0, nil
		}
		if reply != nil {
			reply(stopErr)
		} else {
			// With no owner left to retry a busy mount, disconnect and finish.
			disconnect(stopErr)
		}
	}
}

// stopFTPMount leaves a busy mount connected after a normal unmount refusal.
// It interrupts transport on timeout or transport failure before the forced wait.
func stopFTPMount(ctx context.Context, m ftpMount, disconnect func(error), grace time.Duration) (bool, error) {
	reason := context.Cause(ctx)
	wait := grace
	if reason != nil {
		disconnect(reason)
		wait = 10 * time.Second
	}
	result := make(chan error, 1)
	go func() { result <- m.Unmount() }()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	if reason == nil {
		select {
		case err := <-result:
			if cause := context.Cause(ctx); cause != nil {
				return true, errors.Join(cause, err)
			}
			return err == nil, err
		case <-ctx.Done():
			reason = context.Cause(ctx)
		case <-timer.C:
			reason = errors.New("unmount timed out")
		}
		timer.Stop()
		disconnect(reason)
		timer.Reset(5 * time.Second)
	}
	select {
	case e := <-result:
		if e != nil {
			return true, fmt.Errorf("%w; unmount failed: %w", reason, e)
		}
	case <-timer.C:
		return true, fmt.Errorf("%w; unmount still pending; in-flight writes may be incomplete", reason)
	}
	return true, fmt.Errorf("%w; in-flight writes may be incomplete", reason)
}

type MountApproval struct {
	Instance string `json:",omitempty"`
	Session  string
	PID      int
	ID       string
	Path     string
	ReadOnly bool
}

type message struct {
	HomeDirectory string         `json:"home_directory,omitempty"`
	Account       string         `json:"account,omitempty"`
	Approval      *MountApproval `json:"mount_approval,omitempty"`
	PeerMetadata  jsontext.Value `json:"peer_metadata,omitempty"`

	Type            string `json:"type"`
	SoftwareVersion string `json:"software_version,omitempty"`
	Error           string `json:"error,omitempty"`
	Directory       string `json:"directory,omitempty"`
	Name            string `json:"name,omitempty"`
}

const (
	mountSFTPDataLength = 64 * 1024
	maxMountMessage     = 64*1024 + 4096
)

type mountStream struct {
	io.ReadWriteCloser
	reader *bufio.Reader
}

func newMountStream(c io.ReadWriteCloser) *mountStream {
	return &mountStream{ReadWriteCloser: c, reader: bufio.NewReader(c)}
}

func (c *mountStream) Read(b []byte) (int, error)           { return c.reader.Read(b) }
func (c *mountStream) ReadByte() (byte, error)              { return c.reader.ReadByte() }
func (c *mountStream) ReadSlice(delim byte) ([]byte, error) { return c.reader.ReadSlice(delim) }

func writeMessage(w io.Writer, m message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(body) > maxMountMessage {
		return fmt.Errorf("mount message exceeds %d bytes", maxMountMessage)
	}
	return writeFull(w, append(body, '\n'))
}

func readMessage(r io.Reader) (message, error) {
	var m message
	reader, ok := r.(interface{ ReadByte() (byte, error) })
	if !ok {
		reader = mountByteReader{r}
	}
	var body []byte
	for {
		b, err := reader.ReadByte()
		if err != nil {
			if err == io.EOF && len(body) != 0 {
				err = io.ErrUnexpectedEOF
			}
			return m, err
		}
		if b == '\n' {
			break
		}
		if len(body) == maxMountMessage {
			return m, fmt.Errorf("mount message exceeds %d bytes", maxMountMessage)
		}
		body = append(body, b)
	}
	err := json.Unmarshal(body, &m)
	if err == nil && m.Type == "" {
		return m, fmt.Errorf("empty mount control type")
	}
	return m, err
}

type mountByteReader struct{ io.Reader }

func (r mountByteReader) ReadByte() (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r.Reader, b[:])
	return b[0], err
}

func serveMountData(data io.ReadWriteCloser) error {
	server := sftp.Server{Handler: &mountSFTPHandler{}, Debug: io.Discard, MaxDataLength: mountSFTPDataLength}
	return server.Serve(data)
}

type mountSFTPHandler struct{ localfs.ServerHandler }

type ftpRequestTotals struct {
	Started, Completed, Errors uint64
	SentBytes, ReceivedBytes   uint64
	TotalNS, MaxNS             int64
}
type ftpCallbackTotals struct {
	Calls          uint64
	TotalNS, MaxNS int64
}
type ftpCallback uint8

const (
	ftpCallbackOpen ftpCallback = iota
	ftpCallbackCreate
	ftpCallbackClose
	ftpCallbackRename
	ftpCallbackSetBasicInfo
	ftpCallbackReadDirectory
	ftpCallbackGetFileInfo
	ftpCallbackCount
)

var ftpCallbackNames = [ftpCallbackCount]string{
	ftpCallbackOpen:          "Open",
	ftpCallbackCreate:        "Create",
	ftpCallbackClose:         "Close",
	ftpCallbackRename:        "Rename",
	ftpCallbackSetBasicInfo:  "SetBasicInfo",
	ftpCallbackReadDirectory: "ReadDirectory",
	ftpCallbackGetFileInfo:   "GetFileInfo",
}

type ftpCallbackMetrics struct {
	mu     sync.Mutex
	totals [ftpCallbackCount]ftpCallbackTotals
}

func (m *ftpCallbackMetrics) observe(callback ftpCallback) func() {
	if m == nil {
		return func() {}
	}
	start := time.Now()
	return func() {
		elapsed := int64(time.Since(start))
		m.mu.Lock()
		defer m.mu.Unlock()
		v := &m.totals[callback]
		v.Calls++
		v.TotalNS += elapsed
		v.MaxNS = max(v.MaxNS, elapsed)
	}
}

type MountStats struct {
	Requests                                                   map[string]ftpRequestTotals
	Inflight, PeakInflight                                     int
	Cache                                                      ftpCacheCounters
	CacheBytes, MetadataEntries, Directories, DirectoryEntries int
	Callbacks                                                  map[string]ftpCallbackTotals
}

func collectMountStats(w *ftpWire, f *ftpFS) MountStats {
	report := MountStats{Requests: make(map[string]ftpRequestTotals), Callbacks: make(map[string]ftpCallbackTotals)}
	var requests [256]ftpRequestTotals
	w.mu.Lock()
	if w.totals != nil {
		requests = *w.totals
	}
	report.Inflight, report.PeakInflight = len(w.requests), w.peakRequests
	w.mu.Unlock()
	for k, v := range requests {
		if v.Started != 0 {
			report.Requests[ftpRequestName(byte(k))] = v
		}
	}
	f.cache.mu.Lock()
	report.Cache = f.cache.counters
	report.CacheBytes, report.MetadataEntries, report.Directories, report.DirectoryEntries = f.cache.bytes, f.cache.metadataEntriesLocked(), f.cache.dirs, f.cache.dirEntries
	f.cache.mu.Unlock()
	if f.metrics != nil {
		f.metrics.mu.Lock()
		callbacks := f.metrics.totals
		f.metrics.mu.Unlock()
		for k, v := range callbacks {
			if v.Calls != 0 {
				report.Callbacks[ftpCallbackNames[k]] = v
			}
		}
	}
	return report
}
func startFTPStats(w *ftpWire, f *ftpFS, report func(MountStats)) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				report(collectMountStats(w, f))
				return
			case <-ticker.C:
				report(collectMountStats(w, f))
			}
		}
	}()
	return func() { close(stop); <-done }
}

var ftpRequestNames = [256]string{
	3: "OPEN", 4: "CLOSE", 5: "READ", 6: "WRITE", 7: "LSTAT", 8: "FSTAT",
	9: "SETSTAT", 10: "FSETSTAT", 11: "OPENDIR", 12: "READDIR", 13: "REMOVE",
	14: "MKDIR", 15: "RMDIR", 16: "REALPATH", 17: "STAT", 18: "RENAME",
	19: "READLINK", 20: "SYMLINK", 200: "EXTENDED",
}

func ftpRequestName(kind byte) string {
	if name := ftpRequestNames[kind]; name != "" {
		return name
	}
	return fmt.Sprintf("TYPE_%d", kind)
}

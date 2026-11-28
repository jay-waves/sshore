package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/files/dirlist"
	"github.com/jay-waves/sshore/internal/release"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
	bolt "go.etcd.io/bbolt"
)

const defaultMountIOTimeout = time.Minute

// Direct and locally approved mounts use the same worker configuration.
type ftpOptions struct {
	ApprovalID                          string
	ResumePID                           int
	Connection                          *recentConnection
	IOTimeoutNanos, StartupTimeoutNanos int64
	Host, Remote, Mountpoint            string
	SSH                                 []string
	ReadOnly, DirectIO, Stats           bool
	PickRemote, PickMountpoint          bool
	Name                                string
}

func validateMountConfig(o ftpOptions) error {
	if err := validSSHDestination(o.Host); err != nil {
		return err
	}
	if o.Host == "here" || o.Host == "origin" || (o.Remote == "" && !o.PickRemote) || strings.ContainsRune(o.Remote, 0) || strings.ContainsRune(o.Mountpoint, 0) {
		return fmt.Errorf("invalid mount endpoints")
	}
	if o.IOTimeoutNanos < 0 || o.StartupTimeoutNanos < 0 {
		return fmt.Errorf("invalid mount timeout")
	}
	if o.Connection != nil || o.ApprovalID != "" || o.ResumePID != 0 {
		if o.PickRemote || o.PickMountpoint {
			return fmt.Errorf("approved mounts already specify their paths")
		}
		if o.Connection == nil || !validTaskUID(o.Connection.Instance) || o.ResumePID <= 0 || !validRequestID(o.ApprovalID) || o.Connection.Host != o.Host {
			return fmt.Errorf("invalid mount approval")
		}
		return validateRecentConnection(*o.Connection)
	}
	if len(o.SSH) != 0 {
		_, _, err := parseSSHOptions(o.SSH)
		return err
	}
	return nil
}

func validateLocalMountpoint(path string) (string, error) {
	path, err := files.ResolveMountpoint(path)
	if err != nil {
		return "", err
	}
	return path, files.ValidateMountpoint(path)
}

func readMountApprovalPath(input reviewPromptReader, hint string) (string, error) {
	return readReviewPath(input, "Mountpoint (.: automatic): ", cmp.Or(hint, ".:"), true, func(path string) (string, error) {
		if path == ".:" {
			return "", nil
		}
		return validateLocalMountpoint(path)
	})
}

var mountHistoryBucket = []byte("mount-visits")

// Mount workers read history once and merge their in-memory visits on unmount.
func mountVisitHistory(account, root string, delta files.VisitHistory) files.VisitHistory {
	if !validMachineID(account) {
		return nil
	}
	state, err := statePath("")
	if err != nil {
		return nil
	}
	key := sha256.Sum256([]byte(account + "\x00" + root))
	const maxHistoryBytes = files.VisitHistoryLimit * (6*4096 + 128)
	var history files.VisitHistory
	err = historyTransaction(state, len(delta) > 0, func(tx *bolt.Tx) error {
		bucket := tx.Bucket(mountHistoryBucket)
		if bucket != nil {
			data := bucket.Get(key[:])
			if len(data) > 0 && len(data) <= maxHistoryBytes {
				if err := json.Unmarshal(data, &history); err != nil || len(history) > files.VisitHistoryLimit {
					history = nil
				}
			}
		}
		if history == nil {
			history = files.VisitHistory{}
		}
		history.Merge(delta, time.Now())
		if len(delta) == 0 {
			return nil
		}
		if len(history) == 0 {
			if bucket != nil {
				return bucket.Delete(key[:])
			}
			return nil
		}
		data, err := marshalBoundedStateJSON("mount visit history", history, maxHistoryBytes)
		if err != nil {
			return err
		}
		bucket, err = tx.CreateBucketIfNotExists(mountHistoryBucket)
		if err != nil {
			return err
		}
		return bucket.Put(key[:], data)
	})
	if err != nil {
		return nil
	}
	return history
}

func executeMount(o ftpOptions) (int, error) {
	if o.PickRemote || o.PickMountpoint {
		if err := requireFileSelectionTerminal(); err != nil {
			return 2, err
		}
	}
	if err := files.CheckMountPlatform(); err != nil {
		return 1, err
	}
	return startResourceBackground(resourceConfig{Mount: &o})
}

func runFTPMount(o ftpOptions, s *resourceService, lifecycle files.MountLifecycle, selector func(context.Context, files.DirectoryReader, string) (string, string, error)) (int, error) {
	if !release.Valid(version) {
		return 1, fmt.Errorf("mount requires a full sshore release, got %q", version)
	}
	c := files.MountConfig{
		Remote: o.Remote, Mountpoint: o.Mountpoint, Version: version, Name: o.Name,
		ReadOnly: o.ReadOnly, DirectIO: o.DirectIO, StartupTimeout: time.Duration(o.StartupTimeoutNanos), IOTimeout: time.Duration(o.IOTimeoutNanos),
		History: mountVisitHistory,
		NotificationsUnavailable: func() {
			fmt.Fprintln(os.Stderr, "File notifications unavailable; using short-lived metadata caches.")
		},
	}
	if o.Stats {
		c.ReportStats = func(stats files.MountStats) {
			_ = json.MarshalEncode(jsontext.NewEncoder(os.Stderr), struct {
				Type string
				files.MountStats
			}{Type: "mount_stats", MountStats: stats})
		}
	}
	c.PeerMetadata, _ = json.Marshal(s.peerTask("export " + s.record.Remote))
	if o.Connection != nil {
		c.Approval = &files.MountApproval{Session: o.Connection.Session, PID: o.ResumePID, ID: o.ApprovalID, Instance: o.Connection.Instance, Path: o.Remote, ReadOnly: o.ReadOnly}
		c.Open = func(ctx context.Context) (io.ReadWriteCloser, error) {
			request := approvedResourceRequest(o.Connection.Session, o.ResumePID, o.ApprovalID, operation{Kind: "mount", Path: o.Remote, ReadOnly: o.ReadOnly})
			return openIndependentResource(ctx, *o.Connection, request, s)
		}
		return files.RunMount(c, lifecycle)
	}
	connection := sshConnectionOptions(options{host: o.Host, ssh: o.SSH})
	auth := s.auth
	connection.Environment = s.baseEnvironment()
	connection.Directory = s.directory
	connection.Confirm, connection.Secret = auth.callbacks()
	if o.PickRemote || o.PickMountpoint {
		c.SelectPaths = selector
	}
	connection = withSSHDiagnostics(connection, os.Stderr, auth)
	watchConnection := remoteService(connection, remoteServiceRequest{Operation: "mount-agent", Mode: "mount_watch"})
	connection = remoteService(connection, remoteServiceRequest{Operation: "mount-agent", Mode: "mount_data", PeerMetadata: c.PeerMetadata})
	var data, watch *sshio.Process
	defer func() {
		if data != nil {
			_ = data.Close()
			_ = data.Wait()
		}
		if watch != nil {
			_ = watch.Wait()
		}
	}()
	c.Open = func(ctx context.Context) (io.ReadWriteCloser, error) {
		var err error
		data, err = sshio.Start(ctx, connection, os.Stderr)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	c.OpenNotifications = func(ctx context.Context) (io.ReadWriteCloser, error) {
		var err error
		watch, err = data.OpenSibling(ctx, watchConnection, os.Stderr)
		if err != nil {
			return nil, err
		}
		return watch, nil
	}
	return files.RunMount(c, lifecycle)
}

func runDaemonMount(config resourceConfig, enc *jsontext.Encoder, dec *jsontext.Decoder, c net.Conn, log io.Writer, request localDaemonRequest, tasks *daemonTasks) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon", "mount")
	sys.DetachWorker(cmd)
	cmd.Dir = request.Record.Directory
	cmd.Env = request.Env
	cmd.Stderr = log
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	// Keep the read end independent of Cmd.Wait so a control request can
	// consume the worker's final reply after it exits.
	output, childOutput, err := os.Pipe()
	if err != nil {
		return err
	}
	defer output.Close()
	defer childOutput.Close()
	cmd.Stdout = childOutput
	childInput := jsontext.NewEncoder(input)
	child := jsontext.NewDecoder(output)
	owner, releaseWorker := context.WithCancel(context.Background())
	defer releaseWorker()
	// Claim the task name before the worker can prepare its mountpoint.
	record := resourceRecord{Kind: "mount", UID: config.UID, Name: config.Mount.Name, Host: config.Mount.Host, ReadOnly: config.Mount.ReadOnly, Direction: "<-"}
	task, err := tasks.registerResource(record, releaseWorker)
	if err != nil {
		return err
	}
	config.Mount.Name = task.taskName()
	if err = cmd.Start(); err != nil {
		tasks.finishResource(task)
		return err
	}
	childOutput.Close()
	go func() {
		_ = cmd.Wait()
		tasks.finishResource(task)
	}()
	killWorker := func() {
		_ = cmd.Process.Kill()
		_ = input.Close()
		_ = output.Close()
	}
	context.AfterFunc(owner, func() {
		input.Close()
		timer := time.NewTimer(resourceStopTimeout)
		defer timer.Stop()
		select {
		case <-task.done:
		case <-timer.C:
			killWorker()
		}
	})
	defer func() {
		releaseWorker()
		<-task.done
		// The worker has exited. Let any control call consume its final reply
		// before closing the read end.
		task.stopMu.Lock()
		output.Close()
		task.stopMu.Unlock()
	}()
	// Watch the initiator before writing to the worker, so cancellation also
	// interrupts a blocked configuration write.
	commit := make(chan error, 1)
	configured := make(chan struct{})
	go func() {
		read := func(value any) error { return json.UnmarshalDecode(dec, value) }
		if config.Mount.PickRemote || config.Mount.PickMountpoint {
			err := readMountSelection(read, func(request mountPathRequest) error {
				<-configured
				return json.MarshalEncode(childInput, request)
			})
			if err != nil {
				releaseWorker()
				commit <- err
				return
			}
		}
		commit <- readResourceCommit(read, releaseWorker)
	}()
	defer func() { _ = c.SetReadDeadline(time.Now()) }()
	if err = json.MarshalEncode(childInput, config); err != nil {
		close(configured)
		return err
	}
	close(configured)
	var startup resourceStartup
	for {
		startup = resourceStartup{}
		if err = json.UnmarshalDecode(child, &startup); err != nil {
			return err
		}
		if !startup.Browse && startup.Listing == nil {
			break
		}
		if err = json.MarshalEncode(enc, startup); err != nil {
			return err
		}
	}
	if startup.Error != "" {
		return json.MarshalEncode(enc, startup)
	}
	record = tasks.resourceReady(task, startup.Detail, startup.Remote, startup.Mountpoint)
	if err = offerResourceStartup(enc, record, commit, task.done); err != nil {
		return err
	}
	// The CLI has committed. Task control now owns the worker pipes.
	task.stopMu.Lock()
	if owner.Err() != nil || task.expired() {
		task.stopMu.Unlock()
		return fmt.Errorf("mount stopped during startup")
	}
	task.stop = func() error {
		// A hung worker cannot keep daemon task control blocked on its pipe.
		timer := time.AfterFunc(resourceStopTimeout, killWorker)
		defer timer.Stop()
		if err := json.MarshalEncode(childInput, "kill"); err != nil {
			return err
		}
		var reply resourceReply
		if err := json.UnmarshalDecode(child, &reply); err != nil {
			return fmt.Errorf("mount worker stopped without an unmount result: %w", err)
		}
		if reply.Error != "" {
			return errors.New(reply.Error)
		}
		releaseWorker()
		return nil
	}
	task.stopMu.Unlock()
	acknowledgeResourceStartup(enc, c)
	<-task.done
	return nil
}

func runMountDaemon() (code int, resultErr error) {
	enc := jsontext.NewEncoder(os.Stdout)
	reportedReady := false
	defer func() {
		if !reportedReady && resultErr != nil {
			_ = json.MarshalEncode(enc, resourceStartup{Error: cleanText(resultErr.Error())})
		}
	}()

	// The daemon encodes one JSON record per line. Bound each record so a
	// long-lived control stream never exhausts a cumulative input budget.
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(nil, 1<<20)
	read := func(value any) error {
		if input.Scan() {
			return json.Unmarshal(input.Bytes(), value)
		}
		if err := input.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	var config resourceConfig
	if e := read(&config); e != nil {
		return 1, e
	}
	if config.kind() != "mount" || !validTaskUID(config.UID) {
		return 1, fmt.Errorf("mount worker requires a registered mount configuration")
	}
	o := *config.Mount
	if err := validSession(o.Name); err != nil {
		return 1, err
	}
	if err := validateMountConfig(o); err != nil {
		return 1, err
	}
	record := resourceRecord{UID: config.UID, Kind: "mount", Name: o.Name, Host: o.Host, ReadOnly: o.ReadOnly, Direction: "<-"}
	s := &resourceService{record: record, auth: config.Auth}
	ownerDone := make(chan struct{})
	workerDone := make(chan struct{})
	defer close(workerDone)
	requests := make(chan mountPathRequest)
	unmount := make(chan func(error))
	go func() {
		defer close(ownerDone)
		if o.PickRemote || o.PickMountpoint {
			err := readMountSelection(read, func(request mountPathRequest) error {
				select {
				case requests <- request:
					return nil
				case <-workerDone:
					return io.ErrClosedPipe
				}
			})
			if err != nil {
				return
			}
		}
		for {
			var action string
			if err := read(&action); err != nil || action != "kill" {
				return
			}
			select {
			case unmount <- func(err error) {
				var reply resourceReply
				if err != nil {
					reply.Error = err.Error()
				}
				_ = json.MarshalEncode(enc, reply)
			}:
			case <-workerDone:
				return
			}
		}
	}()
	ready := func(root, mountpoint string) error {
		s.record.Remote, s.record.Mountpoint = root, mountpoint
		if err := json.MarshalEncode(enc, s.record); err != nil {
			return err
		}
		reportedReady = true
		return nil
	}
	lifecycle := files.MountLifecycle{OwnerDone: ownerDone, Ready: ready, Stop: unmount}
	selector := func(ctx context.Context, readDir files.DirectoryReader, home string) (string, string, error) {
		if o.PickRemote {
			go readDir(ctx, home)
		}
		if err := json.MarshalEncode(enc, resourceStartup{Browse: true, Home: home}); err != nil {
			return "", "", err
		}
		for {
			select {
			case request := <-requests:
				if request.Directory == nil {
					return request.Remote, request.Local, nil
				}
				listing := &mountPathListing{}
				listing.Entries, listing.Error = listingEntries(readDir(ctx, *request.Directory))
				if request.Parent != "" {
					listing.ParentEntries, listing.ParentError = listingEntries(readDir(ctx, request.Parent))
				}
				if err := json.MarshalEncode(enc, resourceStartup{Listing: listing}); err != nil {
					return "", "", err
				}
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-ownerDone:
				return "", "", fmt.Errorf("mount startup abandoned")
			}
		}
	}
	return runFTPMount(o, s, lifecycle, selector)
}

type mountPathRequest struct {
	Directory *string
	Parent    string
	Remote    string
	Local     string
}

type mountPathListing struct {
	Entries       []dirlist.Entry
	Error         string
	ParentEntries []dirlist.Entry
	ParentError   string
}

func readMountSelection(read func(any) error, selectPath func(mountPathRequest) error) error {
	for {
		var raw jsontext.Value
		if err := read(&raw); err != nil {
			return err
		}
		var request mountPathRequest
		if raw.Kind() != '{' || json.Unmarshal(raw, &request) != nil {
			return errors.New("invalid mount selection request")
		}
		if err := selectPath(request); err != nil {
			return err
		}
		if request.Directory == nil {
			return nil
		}
	}
}

func (o ftpOptions) selectMountPaths(ctx context.Context, console *termui.LocalConsole, home string, read directoryPairReader) (string, string, error) {
	remote, local := o.Remote, o.Mountpoint
	if o.PickRemote {
		var err error
		picker := pairedFileSelection(home, "Mount directory", o.Host, true, read)
		remote, err = console.PickLF(ctx, picker)
		if err != nil {
			return "", "", err
		}
	}
	if o.PickMountpoint {
		var err error
		local, err = pickLocalMountpoint(ctx, console, "")
		if err != nil {
			return "", "", err
		}
	}
	return remote, local, nil
}

func pickLocalMountpoint(ctx context.Context, console *termui.LocalConsole, dir string) (string, error) {
	picker := termui.LocalFileSelection(dir, "Mountpoint", true)
	picker.Manual = true
	picker.Validate = files.ValidateMountpoint
	point, err := console.PickLF(ctx, picker)
	if !errors.Is(err, termui.ErrLFManual) {
		return point, err
	}
	input, err := newLocalInput()
	if err != nil {
		return "", err
	}
	point, err = readReviewPath(input, "Mountpoint: ", "", false, validateLocalMountpoint)
	return point, errors.Join(err, input.Close())
}

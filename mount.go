package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

const defaultMountIOTimeout = time.Minute

// Direct and locally approved mounts use the same worker configuration.
type ftpOptions struct {
	Approval                  *files.MountApproval
	Connection                *recentConnection
	IOTimeout, StartupTimeout time.Duration
	Host, Remote, Mountpoint  string
	SSH                       []string
	ReadOnly, DirectIO, Stats bool
	Name                      string
	foreground                bool
}

func validateMountConfig(o ftpOptions) error {
	if err := validSSHDestination(o.Host); err != nil {
		return err
	}
	if o.Host == "here" || o.Host == "origin" || o.Remote == "" || strings.ContainsRune(o.Remote, 0) || strings.ContainsRune(o.Mountpoint, 0) {
		return fmt.Errorf("invalid mount endpoints")
	}
	if o.IOTimeout < 0 || o.StartupTimeout < 0 {
		return fmt.Errorf("invalid mount timeout")
	}
	approval := o.Approval
	if approval != nil && (o.Connection == nil || !validTaskUID(approval.Instance) || approval.PID <= 0 || validSession(approval.Session) != nil || !validRequestID(approval.ID) || approval.Path != o.Remote || approval.ReadOnly != o.ReadOnly) {
		return fmt.Errorf("invalid mount approval")
	}
	if connection := o.Connection; connection != nil {
		if approval == nil || connection.Session != approval.Session || connection.Instance != approval.Instance || connection.Host != o.Host {
			return fmt.Errorf("invalid mount approval connection")
		}
		return validateRecentConnection(*connection)
	}
	if len(o.SSH) != 0 {
		_, _, err := parseSSHOptions(o.SSH)
		return err
	}
	return nil
}

func absoluteLocalPath(path string) (string, error) {
	path, err := termui.ExpandHomePath(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func readMountApprovalPath(input reviewPromptReader, hint string) (string, error) {
	return readReviewPath(input, "Mountpoint (empty for automatic): ", hint, true, func(path string) (string, error) {
		if !(runtime.GOOS == "windows" && len(path) == 2 && path[1] == ':') {
			absolute, err := filepath.Abs(path)
			if err != nil {
				return "", err
			}
			path = absolute
		}
		return path, files.ValidateMountpoint(path)
	})
}

// Merge this mount's observations under the account state lock on unmount.
func mountVisitHistory(account, root string, delta files.VisitHistory) files.VisitHistory {
	const maxHistoryBytes = files.VisitHistoryLimit * (6*4096 + 128)
	if !validMachineID(account) {
		return nil
	}
	dir, err := stateDir("mount-history")
	if err != nil {
		return nil
	}
	lock, err := lockStateFile(filepath.Join(dir, "state.lock"))
	if err != nil {
		return nil
	}
	defer lock.Close()
	file := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(account+"\x00"+root))))
	var h files.VisitHistory
	if err := readStateJSON(file, maxHistoryBytes, &h); err != nil || h == nil || len(h) > files.VisitHistoryLimit {
		h = files.VisitHistory{}
	}
	h.Merge(delta, time.Now())
	if len(delta) > 0 {
		_ = writeBoundedStateJSON(file, h, maxHistoryBytes)
	}
	return h
}

func executeMount(o ftpOptions) (int, error) {
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	if err = files.CheckMountPlatform(); err != nil {
		return 1, err
	}
	if !o.foreground {
		return startResourceBackground(resourceConfig{Mount: &o}, "")
	}
	cleanup, err := prepareFTPPaths(&o)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	return runFTPMount(o, nil, nil)
}

func (s *resourceService) mountPeerTask() peerRecord {
	return s.peerTask("export " + s.record.Remote)
}

func runFTPMount(o ftpOptions, s *resourceService, lifecycle *files.MountLifecycle) (int, error) {
	c := files.MountConfig{
		Approval: o.Approval, Remote: o.Remote, Mountpoint: o.Mountpoint, Version: version,
		ReadOnly: o.ReadOnly, DirectIO: o.DirectIO, StartupTimeout: o.StartupTimeout, IOTimeout: o.IOTimeout,
		History: mountVisitHistory,
		Mounted: func(root string) {
			fmt.Fprintf(os.Stderr, "Mounted %s -> %s (SFTP). Close files, then Ctrl-C to unmount.\n", cleanText(sshio.FormatEndpoint(o.Host, root)), cleanText(o.Mountpoint))
		},
		Unmounting: func(grace time.Duration) {
			fmt.Fprintf(os.Stderr, "Unmounting (up to %s). Ctrl-C again forces disconnect.\n", grace)
		},
		UnmountFailed: func(err error) {
			fmt.Fprintf(os.Stderr, "Unmount failed: %s. Still connected; close files and retry Ctrl-C.\n", cleanText(err.Error()))
		},
		NotificationsUnavailable: func() {
			fmt.Fprintln(os.Stderr, "File notifications unavailable; using short-lived metadata caches.")
		},
	}
	if o.Stats {
		c.ReportStats = func(stats files.MountStats) {
			_ = json.NewEncoder(os.Stderr).Encode(struct {
				Type string
				files.MountStats
			}{Type: "mount_stats", MountStats: stats})
		}
	}
	if s != nil {
		c.PeerMetadata, _ = json.Marshal(s.mountPeerTask())
	}
	if o.Approval != nil {
		c.Open = func(ctx context.Context) (io.ReadWriteCloser, error) {
			return openIndependentResource(ctx, *o.Connection, message{
				Type: "route_mount", Name: o.Approval.Session, PID: o.Approval.PID, ApprovalID: o.Approval.ID,
				Operations: []operation{{Kind: "mount", Path: o.Remote, ReadOnly: o.ReadOnly}},
			}, s)
		}
		return files.RunMount(c, lifecycle)
	}
	connection := mountSSHOptions(o)
	var auth *sshAuthEndpoint
	if s != nil {
		auth = s.config.Auth
		connection.Environment = s.baseEnvironment()
		connection.Directory = s.directory
		connection.Confirm, connection.Secret = auth.callbacks()
	}
	connection = withSSHDiagnostics(connection, os.Stderr, auth)
	c.Prepare = func(_ context.Context, wire io.ReadWriter) (string, error) {
		return negotiateSetup(wire, wire, o.Host, false)
	}
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
	c.OpenNotifications = func(_ context.Context) (io.ReadWriteCloser, error) {
		var err error
		watch, err = data.OpenSibling(connection.Command, os.Stderr)
		if err != nil {
			return nil, err
		}
		return watch, nil
	}
	return files.RunMount(c, lifecycle)
}

func mountSSHOptions(o ftpOptions) sshio.Options {
	args, server := o.SSH, ""
	if o.Connection != nil {
		args, server = o.Connection.SSH, o.Connection.Server
	}
	return sshio.Options{Host: o.Host, Args: args, Command: remoteCommand(options{command: "mount-agent", host: o.Host, server: server}), Confirm: confirmSSHHost, Secret: promptSSHSecret}
}

func runDaemonMount(config resourceConfig, enc *json.Encoder, dec *json.Decoder, c net.Conn, log io.Writer, request localDaemonRequest, tasks *daemonTasks) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon", "mount")
	detachDaemon(cmd)
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
	finished := make(chan struct{})
	task := &daemonResourceTask{
		record: resourceRecord{Kind: "mount", UID: config.UID, Name: config.Mount.Name, Host: config.Mount.Host, ReadOnly: config.Mount.ReadOnly, Direction: "<-", Version: version},
		done:   finished,
	}
	childInput := json.NewEncoder(input)
	child := json.NewDecoder(output)
	const (
		starting = iota
		cancelled
		running
	)
	var pipeMu sync.Mutex
	phase := starting // Protected by pipeMu.
	cancelStartupLocked := func() {
		if phase == starting {
			phase = cancelled
			// Closing input also interrupts a blocked startup write.
			_ = input.Close()
		}
	}
	task.control = func(action string) resourceReply {
		pipeMu.Lock()
		defer pipeMu.Unlock()
		select {
		case <-finished:
			return resourceReply{Error: "mount worker exited"}
		default:
		}
		if phase != running {
			switch action {
			case "status":
				return resourceReply{Status: resourceStatus{Version: version, Transport: "starting", Target: "unchecked"}}
			case "kill":
				cancelStartupLocked()
				return resourceReply{}
			default:
				return resourceReply{Error: "invalid resource operation"}
			}
		}
		if err := childInput.Encode(action); err != nil {
			return resourceReply{Error: err.Error()}
		}
		var reply resourceReply
		if err := child.Decode(&reply); err != nil {
			return resourceReply{Error: err.Error()}
		}
		return reply
	}
	// Claim the task name before the worker can prepare its mountpoint.
	if err := tasks.register(config.UID, task); err != nil {
		return err
	}
	config.Mount.Name = task.taskName()
	if err = cmd.Start(); err != nil {
		task.finish(finished)
		return err
	}
	childOutput.Close()
	go func() {
		_ = cmd.Wait()
		task.finish(finished)
	}()
	defer func() {
		input.Close()
		timer := time.NewTimer(12 * time.Second)
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			_ = cmd.Process.Kill()
			<-finished
		}
		// The worker has exited. Let any control call consume its final reply
		// before closing the read end.
		pipeMu.Lock()
		output.Close()
		pipeMu.Unlock()
	}()
	// Watch the initiator before writing to the worker, so cancellation also
	// interrupts a blocked configuration write.
	commit := awaitResourceCommit(dec.Decode, func() {
		pipeMu.Lock()
		defer pipeMu.Unlock()
		cancelStartupLocked()
	})
	defer func() { _ = c.SetReadDeadline(time.Now()) }()
	if err = childInput.Encode(config); err != nil {
		return err
	}
	var startup resourceStartup
	if err = child.Decode(&startup); err != nil {
		return err
	}
	if startup.Error != "" {
		return enc.Encode(startup)
	}
	record := task.publishReady(startup.Detail, startup.Remote, startup.Mountpoint)
	if err = offerResourceStartup(enc, record, commit, finished); err != nil {
		return err
	}
	// Startup still owns both codecs. A stop request may close the input pipe
	// at any time, including while the commit write is blocked.
	if err = commitResourceStartup(childInput, child); err != nil {
		return err
	}
	pipeMu.Lock()
	if phase == cancelled {
		pipeMu.Unlock()
		return fmt.Errorf("mount startup cancelled")
	}
	phase = running
	pipeMu.Unlock()
	acknowledgeResourceStartup(enc, c)
	<-finished
	return nil
}

func runMountDaemon() (code int, resultErr error) {
	enc := json.NewEncoder(os.Stdout)
	committed := false
	defer func() {
		if !committed && resultErr != nil {
			_ = enc.Encode(resourceStartup{Error: cleanText(resultErr.Error())})
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
	record := resourceRecord{Kind: "mount", Name: o.Name, Host: o.Host, ReadOnly: o.ReadOnly, Direction: "<-"}
	s := newResourceService(config, record)
	cleanup, err := prepareFTPPaths(&o)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	abandoned := make(chan struct{})
	ack := awaitResourceCommit(read, func() { close(abandoned) })
	unmount := make(chan chan error)
	var replyMu sync.Mutex
	ready := func(root, mountpoint string) error {
		s.setTransport("connected", nil)
		s.record.Remote, s.record.Mountpoint = root, mountpoint
		if err := offerResourceStartup(enc, s.record, ack, nil); err != nil {
			return err
		}
		if err := enc.Encode("started"); err != nil {
			return err
		}
		committed = true
		go s.serveWorkerControl(read, enc, unmount, &replyMu)
		return nil
	}
	defer func() {
		s.finish()
		// Let an in-flight reply finish before returning to the worker entry point.
		replyMu.Lock()
		replyMu.Unlock()
	}()
	lifecycle := &files.MountLifecycle{Abandoned: abandoned, OperationCompleted: s.operationCompleted, Ready: ready, Stop: unmount}
	return runFTPMount(o, s, lifecycle)
}

// The account daemon retains the worker pipes after commit. Losing that owner
// stops the filesystem worker, including when no task-control RPC is in flight.
func (s *resourceService) serveWorkerControl(read func(any) error, enc *json.Encoder, unmount chan<- chan error, replyMu *sync.Mutex) {
	for {
		var action string
		if err := read(&action); err != nil {
			break
		}
		replyMu.Lock()
		select {
		case <-s.done:
			replyMu.Unlock()
			return
		default:
		}
		err := enc.Encode(s.mountControl(action, unmount))
		replyMu.Unlock()
		if err != nil {
			break
		}
	}
	s.mountControl("kill", unmount)
}

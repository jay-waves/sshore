package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/pkg/sftp"
)

// Requests are immutable daemon-owned intents. Review keeps local selections
// separately and consumes the original before executing.

func newApprovedFile(op operation, localPath string, opened *files.Source) fileCopy {
	if op.Kind == "upload" {
		return fileCopy{source: localPath, target: "/target", opened: opened, approval: &op}
	}
	return fileCopy{source: "/source", target: localPath, approval: &op}
}

func (f fileCopy) approvedName() string {
	if f.opened != nil {
		return f.opened.Name()
	}
	if f.approval.Kind == "upload" && f.source != "" {
		return filepath.Base(f.source)
	}
	return f.approval.Name
}

func (f fileCopy) approvedLocalPath() string {
	if f.approval.Kind == "upload" {
		return f.source
	}
	return f.target
}

func validateOperations(ops []operation) error {
	err := validateRequestOperations(ops)
	if errors.Is(err, errForwardPort) {
		return usage("%s", err)
	}
	return err
}

func queueRequest(o options, ops []operation) (requestRecord, error) {
	if err := validateOperations(ops); err != nil {
		return requestRecord{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return requestRecord{}, err
	}
	reservation, err := chooseTaskIdentity("review", "")
	if err != nil {
		return requestRecord{}, err
	}

	connection, err := connectionFromOptions(o)
	if err != nil {
		return requestRecord{}, err
	}
	r := requestRecord{Connection: connection, SoftwareVersion: version, PID: o.shellPID, ID: reservation.uid, Name: reservation.name, Created: time.Now(), Directory: cwd, Quiet: o.quiet, Operations: ops}
	if err := saveRequest(r); err != nil {
		return requestRecord{}, err
	}
	return r, nil
}

// queuedApproval retains the saved local request until the helper exchange ends.
type queuedApproval struct {
	request requestRecord
	token   string
	timeout <-chan time.Time
}

func (a *queuedApproval) pending() bool             { return a.token != "" }
func (a *queuedApproval) matches(token string) bool { return a.pending() && a.token == token }
func (a *queuedApproval) confirm(token string) (requestRecord, bool) {
	if !a.matches(token) {
		return requestRecord{}, false
	}
	request := a.request
	*a = queuedApproval{}
	return request, true
}
func (a *queuedApproval) sendDetach(control attachmentControl) (bool, error) {
	if a.pending() {
		return false, nil
	}
	return true, streamMessage(control, message{Type: "detach"})
}
func (a *queuedApproval) cancel(result error) error {
	if !a.pending() {
		return result
	}
	id := a.request.ID
	*a = queuedApproval{}
	if err := consumeRequest(id); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(result, fmt.Errorf("could not cancel unconfirmed request: %w", err))
	}
	return result
}

func executeReview(a reviewOptions) (code int, resultErr error) {
	// Never invite a user to enter approval input inside a normal remote shell.
	if os.Getenv("SSHORE_SOCKET") != "" || os.Getenv("SSH_CONNECTION") != "" {
		return 1, fmt.Errorf("run review from your local shell after detach")
	}
	var records []requestRecord
	if a.id == "" {
		var err error
		records, err = pendingRequests()
		if err != nil {
			return 1, err
		}
		switch len(records) {
		case 0:
			fmt.Println("No pending approvals")
			return 0, nil
		case 1:
			a.id = records[0].ID
		}
	}
	input, err := newLocalInput()
	if err != nil {
		return 1, err
	}
	defer func() {
		if input == nil {
			return
		}
		if err := input.Close(); err != nil {
			code = 1
			resultErr = errors.Join(resultErr, fmt.Errorf("restore terminal: %w", err))
		}
	}()
	if a.id == "" {
		names := make([]string, len(records))
		labels := make([]string, len(records))
		for i, r := range records {
			names[i] = r.DisplayName()
			source := r.Connection.Host
			if r.Connection.Session != "" {
				source += " / " + r.Connection.Session
			}
			labels[i] = fmt.Sprintf("%s · %s · %s", names[i], source, requestSummary(r.Operations))
		}
		i, err := chooseLocalItem(input, names, labels, "Request (#number/name, /filter; Enter cancels): ", "request")
		if err != nil {
			return 1, err
		}
		a.id = records[i].ID
	}
	// Re-read after selection: an expired or concurrently cancelled request fails closed.
	r, err := findRequest(a.id)
	if err != nil {
		return 1, err
	}
	kind := r.Operations[0].Kind
	title := "Copy"
	switch kind {
	case "mount":
		title = "Mount"
	case "forward":
		title = "Forward"
	}
	termui.Log(os.Stdout, termui.Info, "%s %s · %s / %s\n", title, r.DisplayName(), cleanText(r.Connection.Host), cleanText(r.Connection.Session))

	resource := kind == "mount" || kind == "forward"
	var config resourceConfig
	var approvedFiles []fileCopy
	if resource {
		config, err = selectApprovedResource(input, &r)
	} else {
		approvedFiles, err = selectApprovedFiles(input, r)
		defer closeCopyBatch(approvedFiles)
	}
	if err != nil {
		return 1, err
	}

	// Release the terminal before SSH takes ownership.
	closeErr := input.Close()
	input = nil
	if closeErr != nil {
		return 1, fmt.Errorf("restore terminal: %w", closeErr)
	}
	if err := prepareApprovalDirectory(&r); err != nil {
		return 1, err
	}
	var reservation *taskIdentitySelection
	if resource {
		if config.Forward != nil {
			if err := validateForwardConfig(*config.Forward); err != nil {
				return 1, err
			}
		} else if err := validateMountConfig(*config.Mount); err != nil {
			return 1, err
		}
		reservation, err = chooseTaskIdentityExcept(config.kind(), r.DisplayName(), r.ID)
		if err != nil {
			return 1, err
		}
	}
	if err := consumeRequest(r.ID); err != nil {
		return 1, err
	}
	if reservation != nil {
		return startSelectedResource(config, reservation, r.Connection.Directory)
	}
	if err := runApprovedFiles(r, approvedFiles); err != nil {
		return 1, err
	}
	return 0, nil
}

func prepareApprovalDirectory(r *requestRecord) error {
	if r.Connection.Directory == "" {
		r.Connection.Directory = r.Directory
	}
	for parent := r.Connection.Parent; parent != nil; parent = parent.Parent {
		if parent.Directory == "" {
			parent.Directory = r.Connection.Directory
		}
	}
	info, err := os.Stat(r.Connection.Directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("SSH working directory is no longer a directory")
	}
	return nil
}

func selectApprovedResource(input reviewPromptReader, r *requestRecord) (resourceConfig, error) {
	op := r.Operations[0]
	if op.Kind == "mount" {
		hint, err := approvalPathHint(op.LocalHint)
		if err != nil {
			return resourceConfig{}, err
		}
		if err := files.CheckMountPlatform(); err != nil {
			return resourceConfig{}, err
		}
		mode := "read-write"
		if op.ReadOnly {
			mode = "read-only"
		}
		termui.Log(os.Stdout, termui.Info, "%s · %s\n", cleanText(op.Path), mode)
		mountpoint, err := readMountApprovalPath(input, hint)
		if err != nil {
			return resourceConfig{}, err
		}
		return resourceConfig{Mount: &ftpOptions{
			Host: r.Connection.Host, Remote: op.Path, Mountpoint: mountpoint, ReadOnly: op.ReadOnly,
			IOTimeout:  defaultMountIOTimeout,
			Approval:   &files.MountApproval{Session: r.Connection.Session, PID: r.PID, ID: r.ID, Instance: r.Connection.Instance},
			Connection: &r.Connection,
		}}, nil
	}

	label := "Local listening port"
	remote := loopback(op.ServerPort())
	if op.Reverse {
		label = "Local service port"
		termui.Log(os.Stdout, termui.Info, "%s %s -> local service\n", cleanText(r.Connection.Host), remote)
	} else {
		termui.Log(os.Stdout, termui.Info, "Local listener -> %s %s\n", cleanText(r.Connection.Host), remote)
	}
	port, err := readApprovalPort(input, label, op.LocalHint)
	if err != nil {
		return resourceConfig{}, err
	}
	resolveLocalPort(&op, port)
	f := forwardConfig{
		Name: r.DisplayName(), Host: r.Connection.Host, Server: r.Connection.Server, SSH: r.Connection.SSH,
		Port: op.Port, Target: op.TargetPort, Reverse: op.Reverse, Session: r.Connection.Session,
		ResumePID: r.PID, ApprovalID: r.ID, Instance: r.Connection.Instance,
	}
	if r.Connection.viaShell() {
		f.Connection = &r.Connection
	}
	return resourceConfig{Forward: &f}, nil
}

func approvalPathHint(hint string) (string, error) {
	if hint == "" {
		return "", nil
	}
	path, err := termui.ExpandHomePath(hint)
	if err != nil {
		return "", fmt.Errorf("invalid local path suggestion; request remains pending: %w", err)
	}
	return path, nil
}

// The caller closes all opened sources, including on a partial selection.
func selectApprovedFiles(input reviewPromptReader, r requestRecord) ([]fileCopy, error) {
	approvedFiles := make([]fileCopy, 0, len(r.Operations))
	destinations := make(map[string]bool)
	for _, op := range r.Operations {
		hint, err := approvalPathHint(op.LocalHint)
		if err != nil {
			return approvedFiles, err
		}
		switch op.Kind {
		case "download":
			dest := filepath.Join(r.Directory, op.Name)
			if hint != "" {
				dest = hint
			}
			termui.Log(os.Stdout, termui.Info, "Download %s · %s\n", cleanText(op.Path), displayBytes(op.Size))
			path, e := readApprovalPath(input, "Save as: ", dest, func(path string) (string, error) {
				path, err := files.DownloadDestination(path, op.Name)
				if err == nil && destinations[path] {
					return "", fmt.Errorf("destination already selected: %s", path)
				}
				return path, err
			})
			if e != nil {
				return approvedFiles, e
			}
			approvedFiles = append(approvedFiles, newApprovedFile(op, path, nil))
			destinations[path] = true
		case "upload":
			termui.Log(os.Stdout, termui.Info, "Upload → %s\n", cleanText(op.Path))
			var source *files.Source
			path, e := readApprovalPath(input, "Local file to upload: ", hint, func(path string) (string, error) {
				f, e := files.OpenSource(path)
				if e != nil {
					return "", e
				}
				source = f
				return path, nil
			})
			if e != nil {
				return approvedFiles, e
			}
			approvedFiles = append(approvedFiles, newApprovedFile(op, path, source))
		}
	}
	return approvedFiles, nil
}

func runApprovedFiles(r requestRecord, approvedFiles []fileCopy) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	request := message{Type: "route_files", Name: r.Connection.Session, PID: r.PID, ApprovalID: r.ID}
	o, err := routedResourceOptions(r.Connection, request.Type)
	if err != nil {
		return err
	}
	transports := make(clientTransports)
	defer transports.releaseUnreferenced()
	open := func() (net.Conn, error) {
		return openResourceEndpoint(ctx, &o, request, transports)
	}
	return executeApprovedFiles(ctx, open, approvedFiles, r.Quiet)
}

func requestSummary(ops []operation) string {
	if len(ops) == 1 {
		op := ops[0]
		if op.Kind == "forward" {
			if op.LocalPortPending() {
				return pendingPortDescription(op)
			}
			return describeForward(op.Port, op.TargetPort, op.Reverse)
		}
		return op.Kind + " " + op.Path
	}
	return fmt.Sprintf("%d requested operations", len(ops))
}

// Each approved file uses its own endpoint. The batch stops on the first failure
// and preserves completed files.

func executeApprovedFiles(ctx context.Context, open func() (net.Conn, error), batch []fileCopy, quiet bool) error {
	completed, err := transferCopyBatch(batch, func(i int, file fileCopy) error {
		if !quiet && len(batch) > 1 {
			termui.Log(os.Stderr, termui.Info, "[sshore] File %d/%d: %s %s\n", i+1, len(batch), file.approval.Kind, cleanText(file.approvedName()))
		}
		return executeApprovedFile(ctx, open, file, quiet)
	})
	if err != nil {
		termui.Log(os.Stderr, termui.Error, "[sshore] Transfer: %d done, 1 failed/cancelled, %d not started. Completed files kept.\n", completed, len(batch)-completed-1)
		for i, file := range batch {
			state := "not started"
			if i < completed {
				state = "completed"
			} else if i == completed {
				state = "failed/cancelled"
			}
			termui.Log(os.Stderr, termui.Info, "[sshore] %s: %s (%s)\n", state, cleanText(file.approvedName()), cleanText(file.approvedLocalPath()))
		}
		termui.Log(os.Stderr, termui.Info, "[sshore] Cleanup may leave .sshore-*.part downloads or partial uploads.\n")
		return err
	}
	if len(batch) > 1 {
		termui.Log(os.Stderr, termui.Success, "[sshore] Transferred %d/%d files.\n", len(batch), len(batch))
	}
	return nil
}

func executeApprovedFile(ctx context.Context, open func() (net.Conn, error), file fileCopy, quiet bool) error {
	remote := *file.approval
	remote.LocalHint = ""
	request := message{Type: "approved_transfer", Operations: []operation{remote}}
	name := file.approvedName()
	var result string
	switch remote.Kind {
	case "upload":
		if file.opened == nil {
			return fmt.Errorf("approved upload has no opened source; request again")
		}
		request.Name, request.Size = name, file.opened.Size()
		result = "Uploaded " + file.source + " -> " + remote.Path
	case "download":
		result = "Saved " + file.target
	default:
		return fmt.Errorf("invalid transfer kind")
	}
	stream, err := open()
	if err != nil {
		return err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	st := stream
	_ = st.SetDeadline(time.Now().Add(requestTimeout))
	if err = writeMessage(stream, request); err != nil {
		return err
	}
	m, err := readMessage(stream)
	if err != nil {
		return err
	}
	if m.Type != "sftp" {
		return fileResponseError(m)
	}
	err = withCopySFTP(stream, func(client *sftp.Client) error {
		_ = st.SetDeadline(time.Time{})
		var completed *atomic.Int64
		if !quiet {
			completed = new(atomic.Int64)
			defer reportTransferProgress(remote.Kind, name, file.size(), completed)()
		}
		return file.transfer(ctx, client, completed)
	})
	if err != nil {
		return err
	}
	termui.Log(os.Stderr, termui.Success, "[sshore] %s\n", cleanText(result))
	return nil
}

func reportTransferProgress(kind, name string, size int64, completed *atomic.Int64) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		progress := transferProgress{at: time.Now()}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				termui.Log(os.Stderr, termui.Info, "[sshore] %s %s: %s\n", kind, cleanText(name), progress.sample(now, completed.Load(), size))
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// Rate is a time-smoothed estimate, never a promise of remote persistence.
type transferProgress struct {
	at    time.Time
	bytes int64
	rate  float64
}

func (p *transferProgress) sample(now time.Time, done, total int64) string {
	elapsed := now.Sub(p.at).Seconds()
	if elapsed > 0 {
		rate := float64(max(int64(0), done-p.bytes)) / elapsed
		if p.at.IsZero() {
			rate = 0
		}
		if p.rate == 0 {
			p.rate = rate
		} else {
			p.rate = .3*rate + .7*p.rate
		}
	}
	p.at, p.bytes = now, done
	eta := "unknown"
	if done >= total {
		eta = "0s"
	} else if p.rate >= 1 {
		seconds := float64(total-done) / p.rate
		if seconds < float64((1<<63-1)/int64(time.Second)) {
			eta = (time.Duration(seconds) * time.Second).String()
		}
	}
	return fmt.Sprintf("%s/%s · %s/s · remaining %s", displayBytes(done), displayBytes(total), displayBytes(int64(p.rate)), eta)
}

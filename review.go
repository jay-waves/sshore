package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/pkg/sftp"
)

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

func newApprovalRequest(o options, ops []operation) (requestRecord, error) {
	if err := validateOperations(ops); err != nil {
		return requestRecord{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return requestRecord{}, err
	}
	connection, err := connectionFromOptions(o)
	if err != nil {
		return requestRecord{}, err
	}
	request := requestRecord{Connection: connection, PID: o.shellPID, ID: newTaskUID(), Directory: cwd, Quiet: o.quiet, Operations: ops}
	if !validTaskUID(connection.Instance) || request.PID <= 0 {
		return requestRecord{}, fmt.Errorf("invalid source shell identity")
	}
	if err := validateRecentConnection(connection); err != nil {
		return requestRecord{}, err
	}
	err = prepareApprovalDirectory(&request)
	return request, err
}

type approvedOperation struct {
	request requestRecord
	config  resourceConfig
	files   []fileCopy
}

func selectApprovedOperation(input reviewPromptReader, request requestRecord) (approvedOperation, error) {
	selected := approvedOperation{request: request}
	line, err := input.ReadPrefilled("Approve this request? [y/N] ", false, "")
	if err != nil || !strings.EqualFold(strings.TrimSpace(line), "y") {
		return selected, errors.New("approval cancelled")
	}
	switch request.Operations[0].Kind {
	case "mount", "forward":
		selected.config, err = selectApprovedResource(input, &request)
	default:
		selected.files, err = selectApprovedFiles(input, request)
	}
	return selected, err
}

type localApproval struct {
	request requestRecord
	source  options
}

func (*localApproval) Error() string { return "local approval requested" }

func executeLocalApproval(request requestRecord, source options) (int, bool, error) {
	input, err := newLocalInput()
	if err != nil {
		return 1, true, err
	}
	out := os.Stdout
	if err = (pty.InputState{}).BeginConsole(out); err != nil {
		_ = input.Close()
		return 1, true, err
	}
	termui.WriteConsoleBody(out, approvalDescription(request))
	selection, selectionErr := selectApprovedOperation(input, request)
	defer closeCopyBatch(selection.files)
	resume := true
	if selectionErr == nil {
		if source.temporary && !source.local {
			termui.WriteConsoleBody(out, "Leaving locally ends this remote spawn when its connection closes.")
		}
		resume, selectionErr = chooseApprovalReturn(input)
	}
	displayErr := (pty.InputState{}).EndConsole(out)
	if err := errors.Join(displayErr, input.Close(), selectionErr); err != nil {
		return 1, resume, err
	}
	if err := selection.run(source); err != nil {
		return 1, resume, err
	}
	return 0, resume, nil
}

func chooseApprovalReturn(input prefilledPromptReader) (bool, error) {
	for {
		line, err := input.ReadPrefilled("Return to session after operation? [Y/n] ", false, "")
		if err != nil {
			return true, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y":
			return true, nil
		case "n":
			return false, nil
		}
	}
}

func (a approvedOperation) run(source options) error {
	if a.config.Forward != nil || a.config.Mount != nil {
		if a.config.Forward != nil {
			if err := validateForwardConfig(*a.config.Forward); err != nil {
				return err
			}
		} else if err := validateMountConfig(*a.config.Mount); err != nil {
			return err
		}
		reservation, err := chooseTaskIdentity(a.config.kind(), "")
		if err != nil {
			return err
		}
		_, err = startSelectedResource(a.config, reservation, a.request.Connection.Directory)
		return err
	}
	return runApprovedFiles(a.request, source, a.files)
}

func approvalDescription(request requestRecord) string {
	var body strings.Builder
	fmt.Fprintf(&body, "From %s / %s\n", cleanText(request.Connection.Host), cleanText(request.Connection.Session))
	for _, op := range request.Operations {
		switch op.Kind {
		case "mount":
			mode := "read-write"
			if op.ReadOnly {
				mode = "read-only"
			}
			fmt.Fprintf(&body, "\nMount remote directory %s (%s)", cleanText(op.Path), mode)
		case "forward":
			fmt.Fprintf(&body, "\n%s", pendingPortDescription(op))
		case "download":
			fmt.Fprintf(&body, "\nDownload %s (%s, %s)", cleanText(op.Path), cleanText(op.Name), displayBytes(op.Size))
		case "upload":
			fmt.Fprintf(&body, "\nUpload to %s", cleanText(op.Path))
		}
		if op.LocalHint != "" {
			fmt.Fprintf(&body, "\nSuggested local endpoint: %s", cleanText(op.LocalHint))
		}
	}
	return body.String()
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
	if op.Reverse {
		label = "Local service port"
	}
	port, err := readApprovalPort(input, label, op.LocalHint)
	if err != nil {
		return resourceConfig{}, err
	}
	resolveLocalPort(&op, port)
	f := forwardConfig{
		Host: r.Connection.Host, Server: r.Connection.Server, SSH: r.Connection.SSH,
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
		return "", fmt.Errorf("invalid local path suggestion: %w", err)
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

func runApprovedFiles(r requestRecord, source options, approvedFiles []fileCopy) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	request := message{Type: "route_files", Name: r.Connection.Session, PID: r.PID, ApprovalID: r.ID}
	target, err := routedResourceOptions(r.Connection, request.Type)
	if err != nil {
		return err
	}
	// The route contains resource-safe SSH arguments. Keep the authenticated
	// attachment's transport and lifetime lease wherever the route allows reuse.
	for from, to := &source, &target; from != nil && to != nil; from, to = from.routeParent, to.routeParent {
		to.reusable = from.reusable
	}
	open := func() (net.Conn, error) {
		return openResourceEndpoint(ctx, &target, request, nil)
	}
	return executeApprovedFiles(ctx, open, approvedFiles, r.Quiet)
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

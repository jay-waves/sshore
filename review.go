package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/wire"
	"github.com/xtaci/smux"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

// Requests are immutable daemon-owned intents. Review fills an independent
// memory copy and consumes the original before executing; failures never replay.

func validateOperations(ops []operation) error {
	err := validateRequestOperations(ops)
	if errors.Is(err, errForwardPort) {
		return usage("%s", err)
	}
	return err
}

func queueRequest(o options, ops []operation) (string, error) {
	if err := validateOperations(ops); err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	reservation, err := chooseTaskIdentity("review", "")
	if err != nil {
		return "", err
	}

	id := reservation.uid
	connection, err := connectionFromOptions(o)
	if err != nil {
		return "", err
	}
	r := requestRecord{Connection: connection, SoftwareVersion: version, PID: o.shellPID, ID: id, Name: reservation.name, Created: time.Now(), Directory: cwd, DownloadDir: o.downloadDir, Quiet: o.quiet, Operations: ops}
	if err := saveRequest(r); err != nil {
		return "", err
	}
	return id, nil
}
func validateRequestRecord(r requestRecord) error {
	if err := validateRecentConnection(r.Connection); err != nil {
		return fmt.Errorf("saved connection is incompatible; request it again: %w", err)
	}

	if !validTaskUID(r.ID) || r.PID <= 0 || !validTaskUID(r.Connection.Instance) {
		return fmt.Errorf("invalid review identity")
	}
	if r.Name != "" && validSession(r.Name) != nil {
		return fmt.Errorf("invalid review name")
	}
	if !filepath.IsAbs(r.Directory) {
		return fmt.Errorf("invalid local approval directory")
	}
	return nil
}
func executeReview(a reviewOptions) (code int, resultErr error) {
	// Never invite a user to enter approval input inside a normal remote shell.
	if os.Getenv("SSHORE_SOCKET") != "" || os.Getenv("SSH_CONNECTION") != "" {
		return 1, fmt.Errorf("run review from your local shell after detach")
	}
	var ids, labels []string
	if a.id == "" {
		records, err := pendingRequests()
		if err != nil {
			return 1, err
		}
		for _, r := range records {
			ids = append(ids, r.DisplayName())
			source := r.Connection.Host
			if r.Connection.Session != "" {
				source += " / " + r.Connection.Session
			}
			labels = append(labels, fmt.Sprintf("%s · %s · %s", r.DisplayName(), source, r.summary()))
		}
		if len(ids) == 0 {
			fmt.Println("No pending approvals")
			return 0, nil
		}
		if len(ids) == 1 {
			a.id = ids[0]
		}
	}
	input, err := newLocalInput()
	if err != nil {
		return 1, err
	}
	defer func() {
		if err := input.Close(); err != nil {
			code = 1
			resultErr = errors.Join(resultErr, fmt.Errorf("restore terminal: %w", err))
		}
	}()
	if a.id == "" {
		a.id, err = chooseLocalItem(input, ids, labels, "Request (#number/name, /filter; Enter cancels): ", "request")
		if err != nil {
			return 1, err
		}
	}
	// Re-read after selection: an expired or concurrently cancelled request fails closed.
	r, err := findRequest(a.id)
	if err != nil {
		return 1, err
	}
	if r.Setup != nil {
		termui.Log(os.Stdout, termui.Info, "Setup %s · %s → %s (remote executable %s)\n", cleanText(r.Connection.Host), cleanText(r.Setup.Installed), version, cleanText(r.Setup.Target))
		for _, task := range r.Setup.Tasks {
			termui.Log(os.Stdout, termui.Warning, "  %s %s %s\n", cleanText(task.Name), cleanText(task.Version), cleanText(task.Start))
		}
		if len(r.Setup.Tasks) > 0 {
			fmt.Println("Updating ends these tasks. Unsaved work may be lost.")
		}
		word := "update"
		if r.Setup.Installed == "missing" && len(r.Setup.Tasks) == 0 {
			word = "install"
		}
		answer, err := input.Read("Type "+word+" to approve, or Enter to decline: ", false)
		if err != nil {
			return 1, err
		}
		if answer != word {
			return 0, consumeRequest(r.ID)
		}
		_, err = requestRPC("setup_approve", r.ID, "", nil)
		return 0, err
	}
	title := "Copy"
	if len(r.Operations) == 1 {
		switch r.Operations[0].Kind {
		case "mount":
			title = "Mount"
		case "forward":
			title = "Forward"
		}
	}
	termui.Log(os.Stdout, termui.Info, "%s %s · %s / %s\n", title, r.DisplayName(), cleanText(r.Connection.Host), cleanText(r.Connection.Session))
	for i := range r.Operations {
		op := &r.Operations[i]
		if op.Kind != "forward" && op.LocalHint != "" {
			hint, e := termui.ParsePath([]byte(op.LocalHint))
			if e != nil {
				return 1, fmt.Errorf("invalid local path suggestion; request remains pending: %w", e)
			}
			op.LocalHint = hint
		}
		switch op.Kind {
		case "download":
			dest := r.DownloadDir
			if dest == "" {
				dest = r.Directory
			}
			dest = filepath.Join(dest, op.Name)
			if op.LocalHint != "" {
				dest = op.LocalHint
			}
			termui.Log(os.Stdout, termui.Info, "Download %s · %s\n", cleanText(op.Path), displayBytes(op.Size))
			path, e := readApprovalPath(input, fmt.Sprintf("Save as [%s] (Enter confirms; Ctrl-C cancels): ", cleanText(dest)), dest, func(path string) (string, error) {
				return files.DownloadDestination(path, op.Name)
			})
			if e != nil {
				return 1, e
			}
			op.LocalPath = path
		case "upload":
			termui.Log(os.Stdout, termui.Info, "Upload → %s\n", cleanText(op.Path))
			prompt := "Local file to upload (Enter confirms; Ctrl-C cancels): "
			if op.LocalHint != "" {
				prompt = fmt.Sprintf("Local file to upload [%s] (Enter confirms; Ctrl-C cancels): ", cleanText(op.LocalHint))
			}
			path, e := readApprovalPath(input, prompt, op.LocalHint, func(path string) (string, error) {
				f, e := files.OpenSource(path)
				if e != nil {
					return "", e
				}
				defer f.Close()
				op.Name, op.Size = f.Name(), f.Size()
				return path, nil
			})
			if e != nil {
				return 1, e
			}
			op.LocalPath = path
		case "mount":
			if e := files.CheckMountPlatform(); e != nil {
				return 1, e
			}
			mode := "read-write"
			if op.ReadOnly {
				mode = "read-only"
			}
			termui.Log(os.Stdout, termui.Info, "%s · %s\n", cleanText(op.Path), mode)
			mountpoint, e := readMountApprovalPath(input, op.LocalHint)
			if e != nil {
				return 1, e
			}
			op.LocalPath = mountpoint
		case "forward":
			remote := op.ServerPort()
			label := "Local listening port"
			if op.Reverse {
				label = "Local service port"
			}
			if op.Reverse {
				termui.Log(os.Stdout, termui.Info, "%s %s -> local service\n", cleanText(r.Connection.Host), loopback(remote))
			} else {
				termui.Log(os.Stdout, termui.Info, "Local listener -> %s %s\n", cleanText(r.Connection.Host), loopback(remote))
			}
			port, e := readSelectedPort(input, label, op.LocalHint, "approval cancelled; request remains pending")
			if e != nil {
				return 1, e
			}
			resolveLocalPort(op, port)
		}
	}
	// Duplicate destinations are rejected before executing any batch member.
	destinations := map[string]bool{}
	for i := range r.Operations {
		op := &r.Operations[i]
		if op.Kind == "download" {
			if destinations[op.LocalPath] {
				return 1, fmt.Errorf("duplicate destination; request remains pending: %s", op.LocalPath)
			}
			destinations[op.LocalPath] = true
		}
		op.LocalHint = ""
	}
	// Release the input reader and terminal before SSH takes ownership.
	if err := input.Close(); err != nil {
		return 1, err
	}
	// A user may leave the prompt open past the expiration time.
	if time.Since(r.Created) > requestTTL {
		return 1, fmt.Errorf("approval expired; request it again")
	}
	// Restore the original local working directory for relative SSH configuration
	// paths. Selected local file paths are already absolute.
	connectionDirectory := r.Connection.Directory
	if connectionDirectory == "" {
		connectionDirectory = r.Directory
	}
	if err = os.Chdir(connectionDirectory); err != nil {
		return 1, err
	}
	reservation, err := chooseTaskIdentityExcept("review", r.DisplayName(), r.ID)
	if err != nil {
		return 1, err
	}

	reservation.uid = r.ID
	if err = consumeRequest(r.ID); err != nil {
		return 1, err
	}
	if len(r.Operations) == 1 && r.Operations[0].Kind == "mount" {
		op := r.Operations[0]
		args := []string{remotePathOperand(r.Connection.Host, op.Path)}
		if op.LocalPath != "" {
			args = append(args, op.LocalPath)
		}
		args = append(args, "--server", r.Connection.Server)
		if op.ReadOnly {
			args = append(args, "--read-only")
		}
		if len(r.Connection.SSH) > 0 {
			args = append(args, "--")
			args = append(args, r.Connection.SSH...)
		}
		config := resourceConfig{Args: args, MountApproval: &files.MountApproval{Session: r.Connection.Session, PID: r.PID, ID: r.ID, Instance: r.Connection.Instance}}
		if r.Connection.viaShell() {
			connection := r.Connection
			config.MountConnection = &connection
		}
		return startSelectedResource(config, "mount", reservation)
	}
	if len(r.Operations) == 1 && r.Operations[0].Kind == "forward" {
		op := r.Operations[0]
		f := forwardConfig{Name: r.DisplayName(), Host: r.Connection.Host, Server: r.Connection.Server, SSH: r.Connection.SSH, Port: op.Port, Target: op.TargetPort, Reverse: op.Reverse, Session: r.Connection.Session, ResumePID: r.PID, ApprovalID: r.ID}
		f.Instance = r.Connection.Instance
		if r.Connection.viaShell() {
			connection := r.Connection
			f.Connection = &connection
		}
		if err := validateForwardConfig(f); err != nil {
			return 1, err
		}
		return startSelectedResource(resourceConfig{Forward: &f}, "forward", reservation)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	conn, err := openRoutedResource(ctx, r.Connection, message{Type: "route_files", Name: r.Connection.Session, PID: r.PID, ApprovalID: r.ID})
	if err != nil {
		return 1, err
	}
	defer conn.Close()
	mux, err := smux.Client(conn, wire.MuxConfig())
	if err != nil {
		return 1, err
	}
	defer mux.Close()
	if err := executeApprovedFiles(ctx, mux, r.Operations, r.Quiet); err != nil {
		return 1, err
	}
	return 0, nil
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

// Approved files execute sequentially; the first failure stops the batch without
// rolling back completed files. Ctrl-C cancels local file operations; a failed
// transfer closes the attachment while preserving its shell.
// File execution uses an independent stream. Approved forwarding runs separately
// as an independent background resource without attaching to the shell.

func executeApprovedFiles(ctx context.Context, mux *smux.Session, ops []operation, quiet bool) error {
	var files []operation
	for _, op := range ops {
		if op.Kind != "forward" {
			files = append(files, op)
		}
	}
	for i, op := range files {
		if !quiet && len(files) > 1 {
			termui.Log(os.Stderr, termui.Info, "[sshore] File %d/%d: %s %s\n", i+1, len(files), op.Kind, cleanText(op.Name))
		}
		if err := executeApprovedFile(ctx, mux, op, quiet); err != nil {
			termui.Log(os.Stderr, termui.Error, "[sshore] Transfer: %d done, 1 failed/cancelled, %d not started. Completed files kept.\n", i, len(files)-i-1)
			for j, file := range files {
				state := "not started"
				if j < i {
					state = "completed"
				} else if j == i {
					state = "failed/cancelled"
				}
				termui.Log(os.Stderr, termui.Info, "[sshore] %s: %s (%s)\n", state, cleanText(file.Name), cleanText(file.LocalPath))
			}
			termui.Log(os.Stderr, termui.Info, "[sshore] Cleanup may leave .sshore-*.part downloads or partial uploads.\n")
			return err
		}
	}
	if len(files) > 1 {
		termui.Log(os.Stderr, termui.Success, "[sshore] Transferred %d/%d files.\n", len(files), len(files))
	}
	return nil
}

func executeApprovedFile(ctx context.Context, mux *smux.Session, op operation, quiet bool) error {
	st, err := mux.OpenStream()
	if err != nil {
		return err
	}
	defer st.Close()
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	remote := op
	remote.LocalPath = ""
	if op.Kind == "upload" {
		f, err := files.OpenSource(op.LocalPath)
		if err != nil {
			return err
		}
		f.Close()
		if f.Name() != op.Name || f.Size() != op.Size {
			return fmt.Errorf("local file metadata changed; request again")
		}
		remote.Name, remote.Size = "", 0
	}
	if err = writeMessage(st, message{Type: "approved_transfer", Operations: []operation{remote}, Name: op.Name, Size: op.Size}); err != nil {
		return err
	}
	m, err := readMessage(st)
	if err != nil {
		return err
	}
	if m.Type != "sftp" {
		return fileResponseError(m)
	}
	client, err := files.NewSFTP(st, st)
	if err != nil {
		return err
	}
	defer func() { st.Close(); client.Close() }()
	_ = st.SetDeadline(time.Time{})
	var completed atomic.Int64
	done := make(chan error, 1)
	go func() {
		if op.Kind == "upload" {
			done <- files.Upload(client, op.LocalPath, "/target", op.Size, &completed)
		} else {
			done <- files.Download(client, "/source", op.LocalPath, op.Size, &completed)
		}
	}()
	var progress transferProgress
	progress.at = time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				return err
			}
			result := "Saved " + op.LocalPath
			if op.Kind == "upload" {
				result = "Uploaded " + op.LocalPath + " -> " + op.Path
			}
			termui.Log(os.Stderr, termui.Success, "[sshore] %s\n", cleanText(result))
			return nil
		case <-ticker.C:
			if !quiet {
				termui.Log(os.Stderr, termui.Info, "[sshore] %s %s: %s\n", op.Kind, cleanText(op.Name), progress.sample(time.Now(), completed.Load(), op.Size))
			}
		}
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

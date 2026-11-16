package main

// Remote requests describe data, never commands. Only a separately invoked local
// review command can turn a request into executable operations. Records live in
// <UserConfigDir>/sshore/approvals, or under SSHORE_STATE_DIR when overridden. They
// contain connection options and metadata, not collected SSH credentials; platform
// helpers enforce private ownership/permissions or a current-user-only DACL.
// Records expire after ten minutes, are capped at 32, and are consumed before SSH
// reconnects. Each request contains at most 16 operations. Reconnection preserves
// the original options and shell PID; failed execution needs a fresh request.
// Submitting all requested local paths or the local port is consent.
// The input reader is closed before SSH starts so authentication owns stdin.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
)

func validateOperations(ops []operation) error {
	err := validateApprovalOperations(ops)
	if errors.Is(err, errForwardPort) {
		return usage("%s", err)
	}
	return err
}

func findApproval(dir, ref string) (approvalRecord, error) {
	if validApprovalID(ref) {
		return readApproval(dir, ref)
	}
	if validSession(ref) != nil {
		return approvalRecord{}, fmt.Errorf("invalid review name")
	}
	records, err := pendingApprovals(dir)
	if err != nil {
		return approvalRecord{}, err
	}
	var found *approvalRecord
	for _, r := range records {
		if r.Name == ref {
			if found != nil {
				return approvalRecord{}, fmt.Errorf("ambiguous review name %s", ref)
			}
			copy := r
			found = &copy
		}
	}
	if found == nil {
		return approvalRecord{}, fmt.Errorf("review %s does not exist or expired", ref)
	}
	return *found, nil
}

func approvalDir() (string, error) { return stateDir("approvals") }

func writeApproval(o options, ops []operation) (string, error) {
	if err := validateOperations(ops); err != nil {
		return "", err
	}
	dir, err := approvalDir()
	if err != nil {
		return "", err
	}
	if err := approvalStore(dir).CheckCapacity(); err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if o.sshDirectory != "" {
		cwd = o.sshDirectory
	}
	reservation, err := reserveResourceName("review", "")
	if err != nil {
		return "", err
	}
	defer reservation.Close()
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	id := hex.EncodeToString(nonce)
	r := approvalRecord{SoftwareVersion: version, PID: o.resumePID, ID: id, Name: reservation.name, Created: time.Now(), Host: o.host, Session: o.session, Server: o.server, Directory: cwd, DownloadDir: o.downloadDir, SSH: o.ssh, Quiet: o.quiet, NoEscape: o.noEscape, Operations: ops}
	if err := approvalStore(dir).Save(r); err != nil {
		return "", err
	}
	return id, nil
}
func approvalStore(dir string) approvalStorage {
	return approvalStorage{directory: dir}
}
func validateApprovalRecord(r approvalRecord) error {
	if r.Name != "" && validSession(r.Name) != nil {
		return fmt.Errorf("invalid review name")
	}
	if err := validSession(r.Session); err != nil {
		return err
	}
	if !filepath.IsAbs(r.Directory) || r.Host == "" || r.Server == "" {
		return fmt.Errorf("invalid saved connection")
	}
	return nil
}
func readApproval(dir, id string) (approvalRecord, error) {
	record, err := approvalStore(dir).Read(id)
	if errors.Is(err, errForwardPort) {
		err = usage("%s", err)
	}
	return record, err
}
func pendingApprovals(dir string) ([]approvalRecord, error) { return approvalStore(dir).Pending() }
func consumeApproval(dir, id string) error                  { return approvalStore(dir).Consume(id) }

func runReview(args []string) (code int, resultErr error) {
	a, err := parseReview(args)
	if err != nil {
		return 2, err
	}
	return executeReview(a)
}

func executeReview(a reviewOptions) (code int, resultErr error) {
	// Never invite a user to enter approval input inside a normal remote shell.
	if os.Getenv("SSHORE_SOCKET") != "" || os.Getenv("SSH_CONNECTION") != "" {
		return 1, fmt.Errorf("run review from your local shell after detach")
	}
	dir, err := approvalDir()
	if err != nil {
		return 1, err
	}
	var ids, labels []string
	if a.id == "" {
		records, err := pendingApprovals(dir)
		if err != nil {
			return 1, err
		}
		for _, r := range records {
			ids = append(ids, r.DisplayName())
			labels = append(labels, fmt.Sprintf("%s · %s / %s · %s", r.DisplayName(), r.Host, r.Session, approvalSummary(r.Operations)))
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
	r, err := findApproval(dir, a.id)
	if err != nil {
		return 1, err
	}
	if a.stay && len(r.Operations) == 1 && (r.Operations[0].Kind == "forward" || r.Operations[0].Kind == "mount") {
		return 2, usage("--stay applies only to file transfers; mounts and forwards always return locally")
	}
	termui.Log(os.Stdout, termui.Info, "Review %s · %s / %s\n", r.DisplayName(), cleanText(r.Host), r.Session)
	termui.Log(os.Stdout, termui.Info, "Submit all paths or port to start; Ctrl-C cancels.\n")
	if len(r.Operations) == 1 && (r.Operations[0].Kind == "forward" || r.Operations[0].Kind == "mount") {
		termui.Log(os.Stdout, termui.Info, "After: background %s; shell detached.\n", r.Operations[0].Kind)
	} else if a.stay {
		termui.Log(os.Stdout, termui.Info, "After: local.\n")
	} else {
		termui.Log(os.Stdout, termui.Info, "After: attach.\n")
	}
	for i := range r.Operations {
		op := &r.Operations[i]
		if op.Kind != "forward" && op.LocalHint != "" {
			hint, e := localPath([]byte(op.LocalHint))
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
			path, e := readApprovalPath(input, fmt.Sprintf("Save as [%s] (Tab; Enter accepts): ", cleanText(dest)), dest, func(path string) (string, error) {
				return files.DownloadDestination(path, op.Name)
			})
			if e != nil {
				return 1, e
			}
			op.LocalPath = path
			termui.Log(os.Stdout, termui.Info, "→ %s\n", cleanText(path))
		case "upload":
			termui.Log(os.Stdout, termui.Info, "Upload → %s\n", cleanText(op.Path))
			prompt := "Local file to upload (Tab; Enter accepts): "
			if op.LocalHint != "" {
				prompt = fmt.Sprintf("Local file to upload [%s] (Tab; Enter accepts): ", cleanText(op.LocalHint))
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
			termui.Log(os.Stdout, termui.Info, "Source: %s · %s\n", cleanText(path), displayBytes(op.Size))
		case "mount":
			if e := files.CheckMountPlatform(); e != nil {
				return 1, e
			}
			mode := "read-write"
			if op.ReadOnly {
				mode = "read-only"
			}
			termui.Log(os.Stdout, termui.Info, "Mount %s · %s\n", cleanText(op.Path), mode)
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
				termui.Log(os.Stdout, termui.Info, "Server %s -> selected local service\n", loopback(remote))
			} else {
				termui.Log(os.Stdout, termui.Info, "Selected local listener -> server %s\n", loopback(remote))
			}
			port, e := readSelectedPort(input, label, op.LocalHint, "approval cancelled; request remains pending")
			if e != nil {
				return 1, e
			}
			resolveLocalPort(op, port)
			termui.Log(os.Stdout, termui.Info, "Forward %s\n", describeForward(op.Port, op.TargetPort, op.Reverse))
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
	if time.Since(r.Created) > approvalTTL {
		return 1, fmt.Errorf("approval expired; request it again")
	}
	// Restore the original local working directory for relative SSH configuration
	// paths. Selected local file paths are already absolute.
	if err = os.Chdir(r.Directory); err != nil {
		return 1, err
	}
	reservation, err := reserveResourceNameExcept("review", r.DisplayName(), r.ID)
	if err != nil {
		return 1, err
	}
	defer reservation.Close()
	if err = consumeApproval(dir, r.ID); err != nil {
		return 1, err
	}
	if len(r.Operations) == 1 && r.Operations[0].Kind == "mount" {
		op := r.Operations[0]
		args := []string{r.Host, op.Path}
		if op.LocalPath != "" {
			args = append(args, op.LocalPath)
		}
		args = append(args, "--server", r.Server)
		if op.ReadOnly {
			args = append(args, "--read-only")
		}
		if len(r.SSH) > 0 {
			args = append(args, "--")
			args = append(args, r.SSH...)
		}
		return startReservedResource(resourceConfig{Args: args, MountApproval: &files.MountApproval{Session: r.Session, PID: r.PID, ID: r.ID}}, "mount", reservation)
	}
	if len(r.Operations) == 1 && r.Operations[0].Kind == "forward" {
		op := r.Operations[0]
		f := forwardConfig{Name: r.DisplayName(), Host: r.Host, Server: r.Server, SSH: r.SSH, Port: op.Port, Target: op.TargetPort, Reverse: op.Reverse, Session: r.Session, ResumePID: r.PID, ApprovalID: r.ID}
		if err := validateForwardConfig(f); err != nil {
			return 1, err
		}
		return startReservedResource(resourceConfig{Forward: &f}, "forward", reservation)
	}
	return runClient(options{host: r.Host, session: r.Session, server: r.Server, downloadDir: r.DownloadDir, ssh: r.SSH, quiet: r.Quiet, noEscape: r.NoEscape, approved: r.Operations, stay: a.stay, resumePID: r.PID, approvalID: r.ID})
}

func approvalSummary(ops []operation) string {
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

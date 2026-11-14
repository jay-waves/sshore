package main

// Remote requests describe data, never commands. Only a separately invoked local
// approve command can turn a request into executable operations. Records live in
// <UserConfigDir>/sshore/approvals, or under SSHORE_STATE_DIR when overridden. They
// contain connection options and metadata, not collected SSH credentials; platform
// helpers enforce private ownership/permissions or a current-user-only DACL.
// Records expire after ten minutes, are capped at 32, and are consumed before SSH
// reconnects. Each request contains at most 16 operations. Reconnection preserves
// the original options and shell PID; failed execution needs a fresh request.
// Single-upload source submission is consent; other requests require a final yes.
// The input reader is closed before SSH starts so authentication owns stdin.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
)

const approvalTTL = 10 * time.Minute
const maxApprovalItems = 16
const maxPendingApprovals = 32
const maxApprovalRecord = 128 * 1024

type operation struct {
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`
	Name       string `json:"name,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Port       int    `json:"port,omitempty"`
	Reverse    bool   `json:"reverse,omitempty"`
	RemoteHost string `json:"remote_host,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
	LocalPath  string `json:"-"`
}

func validateOperations(ops []operation) error {
	if len(ops) == 0 || len(ops) > maxApprovalItems {
		return fmt.Errorf("request must contain 1–%d operations", maxApprovalItems)
	}
	var total int64
	for _, op := range ops {
		switch op.Kind {
		case "download", "upload":
			// The remote server is Linux, even when the client is Windows.
			if !strings.HasPrefix(op.Path, "/") || len(op.Path) > 4096 || strings.ContainsAny(op.Path, "\x00\r\n") {
				return fmt.Errorf("invalid remote path")
			}
			if op.Port != 0 || op.Reverse {
				return fmt.Errorf("unexpected port in file request")
			}
			if op.Kind == "download" {
				if err := safeName(op.Name); err != nil {
					return err
				}
				if op.Size < 0 || op.Size > (1<<63-1)-total {
					return fmt.Errorf("invalid download size")
				}
				total += op.Size
			} else if op.Name != "" || op.Size != 0 {
				return fmt.Errorf("unexpected upload metadata")
			}
		case "forward":
			if len(ops) != 1 || op.Port < 1 || op.Port > 65535 || (op.RemotePort != 0 && (op.RemotePort < 1 || op.RemotePort > 65535)) || (op.RemoteHost != "" && op.RemoteHost != "127.0.0.1") || op.Path != "" || op.Name != "" || op.Size != 0 {
				return fmt.Errorf("invalid port request")
			}
		default:
			return fmt.Errorf("unsupported requested operation")
		}
	}
	return nil
}

type approvalRecord struct {
	PID                                           int
	SoftwareVersion                               string
	ID                                            string
	Created                                       time.Time
	Host, Session, Server, Directory, DownloadDir string
	SSH                                           []string
	Quiet, NoEscape                               bool
	Operations                                    []operation
}

func approvalDir() (string, error) {
	root := os.Getenv("SSHORE_STATE_DIR")
	if root == "" {
		var err error
		root, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(root, "sshore")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if err = privateState(root, true); err != nil {
		return "", err
	}
	dir := filepath.Join(root, "approvals")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err = privateState(dir, true); err != nil {
		return "", err
	}
	return dir, nil
}

func validApprovalID(id string) bool {
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 16 && id == strings.ToLower(id)
}
func writeApproval(o options, ops []operation) (string, error) {
	if err := validateOperations(ops); err != nil {
		return "", err
	}
	dir, err := approvalDir()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			info, err := e.Info()
			if err != nil {
				return "", err
			}
			if time.Since(info.ModTime()) > approvalTTL {
				_ = os.Remove(filepath.Join(dir, e.Name()))
				continue
			}
			count++
		}
	}
	if count >= maxPendingApprovals {
		return "", fmt.Errorf("too many pending approvals; use 'sshore approve --list' and --reject")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	id := hex.EncodeToString(nonce)
	r := approvalRecord{SoftwareVersion: version, PID: o.resumePID, ID: id, Created: time.Now(), Host: o.host, Session: o.session, Server: o.server, Directory: cwd, DownloadDir: o.downloadDir, SSH: o.ssh, Quiet: o.quiet, NoEscape: o.noEscape, Operations: ops}
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	if len(data) > maxApprovalRecord {
		return "", fmt.Errorf("approval record too large")
	}
	path := filepath.Join(dir, id+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if err = privateState(path, false); err != nil {
		return "", err
	}
	if err = writeFull(f, data); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	ok = true
	return id, nil
}
func readApproval(dir, id string) (approvalRecord, error) {
	var r approvalRecord
	if !validApprovalID(id) {
		return r, fmt.Errorf("invalid approval ID")
	}
	path := filepath.Join(dir, id+".json")
	if err := privateState(path, false); err != nil {
		return r, err
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxApprovalRecord+1))
	if err != nil {
		return r, err
	}
	if len(data) > maxApprovalRecord {
		return r, fmt.Errorf("approval record too large")
	}
	if err = json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if !compatibleVersions(version, r.SoftwareVersion) || r.ID != id || time.Since(r.Created) > approvalTTL || r.Created.After(time.Now().Add(time.Minute)) {
		return r, fmt.Errorf("approval expired or incompatible; request it again")
	}
	if err = validSession(r.Session); err != nil {
		return r, err
	}
	if !filepath.IsAbs(r.Directory) || r.Host == "" || r.Server == "" {
		return r, fmt.Errorf("invalid saved connection")
	}
	return r, validateOperations(r.Operations)
}

// Removing the record atomically claims it before any network or file operation.
// Only one concurrent approver can succeed; failed executions cannot be replayed.
func consumeApproval(dir, id string) error {
	return os.Remove(filepath.Join(dir, id+".json"))
}

type approveOptions struct {
	id                 string
	stay, list, reject bool
}

func parseApprove(args []string) (approveOptions, error) {
	var o approveOptions
	mode := ""
	for _, a := range args {
		switch a {
		case "--stay", "--attach":
			if mode != "" && mode != a {
				return o, usage("--stay and --attach are mutually exclusive")
			}
			mode = a
			o.stay = a == "--stay"
		case "--list":
			o.list = true
		case "--reject":
			o.reject = true
		default:
			a = strings.TrimPrefix(a, "approval/")
			if o.id != "" || !validApprovalID(a) {
				return o, usage("usage: sshore approve [ID] [--attach | --stay] | --list | ID --reject")
			}
			o.id = a
		}
	}
	if o.list && (o.reject || o.id != "" || mode != "") || o.reject && (o.id == "" || mode != "") {
		return o, usage("invalid approve options")
	}
	return o, nil
}
func runApprove(args []string) (code int, resultErr error) {
	a, err := parseApprove(args)
	if err != nil {
		return 2, err
	}
	// Never invite a user to enter approval input inside a normal remote shell.
	if os.Getenv("SSHORE_SOCKET") != "" || os.Getenv("SSH_CONNECTION") != "" {
		return 1, fmt.Errorf("run approve from your local shell after detach")
	}
	dir, err := approvalDir()
	if err != nil {
		return 1, err
	}
	if a.list || a.id == "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return 1, err
		}
		var ids []string
		for _, e := range entries {
			id := strings.TrimSuffix(e.Name(), ".json")
			if id == e.Name() {
				continue
			}
			r, err := readApproval(dir, id)
			if err != nil {
				continue
			}
			ids = append(ids, id)
			if a.list {
				fmt.Printf("%s  %s / %s  %d operation(s)\n", id, cleanText(r.Host), r.Session, len(r.Operations))
			}
		}
		if a.list {
			if len(ids) == 0 {
				fmt.Println("No pending approvals")
			}
			return 0, nil
		}
		if len(ids) != 1 {
			return 1, fmt.Errorf("found %d pending approvals; use 'sshore approve --list', then specify an ID", len(ids))
		}
		a.id = ids[0]
	}
	if a.reject {
		rejected, readErr := readApproval(dir, a.id)
		err = os.Remove(filepath.Join(dir, a.id+".json"))
		if err != nil {
			return 1, err
		}
		if readErr == nil {
			removeRejectedPeerTask(rejected)
		}
		logf(os.Stderr, logSuccess, "[sshore] Request rejected\n")
		return 0, nil
	}
	r, err := readApproval(dir, a.id)
	if err != nil {
		return 1, err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return 1, fmt.Errorf("approval requires a local interactive terminal")
	}
	resetTerminalInputModes()
	if err = flushTerminalInput(); err != nil {
		return 1, err
	}
	logf(os.Stdout, logInfo, "Approve %s · %s / %s\n", r.ID, cleanText(r.Host), r.Session)
	input, err := newApprovalInput()
	if err != nil {
		return 1, err
	}
	defer func() {
		if err := input.Close(); err != nil {
			code = 1
			resultErr = errors.Join(resultErr, fmt.Errorf("restore terminal: %w", err))
		}
	}()
	directUpload := len(r.Operations) == 1 && r.Operations[0].Kind == "upload"
	if len(r.Operations) == 1 && r.Operations[0].Kind == "forward" {
		logf(os.Stdout, logInfo, "After: independent background forward; remote shell stays detached.\n")
	} else if a.stay {
		logf(os.Stdout, logInfo, "After: local.\n")
	} else {
		logf(os.Stdout, logInfo, "After: attach.\n")
	}
	for i := range r.Operations {
		op := &r.Operations[i]
		switch op.Kind {
		case "download":
			dest := r.DownloadDir
			if dest == "" {
				dest = r.Directory
			}
			dest = filepath.Join(dest, op.Name)
			logf(os.Stdout, logInfo, "Download %s · %s\n", cleanText(op.Path), displayBytes(op.Size))
			path, e := readApprovalPath(input, fmt.Sprintf("Save as [%s] (Tab): ", cleanText(dest)), dest, func(path string) (string, error) {
				return downloadDestination(path, op.Name)
			})
			if e != nil {
				return 1, e
			}
			op.LocalPath = path
			logf(os.Stdout, logInfo, "→ %s\n", cleanText(path))
		case "upload":
			logf(os.Stdout, logInfo, "Upload → %s\n", cleanText(op.Path))
			prompt := "Local file to upload (Tab): "
			if directUpload {
				prompt = "Local file to upload (Tab; Enter sends): "
			}
			path, e := readApprovalPath(input, prompt, "", func(path string) (string, error) {
				f, e := openSending(path)
				if e != nil {
					return "", e
				}
				defer f.close()
				op.Name, op.Size = f.name, f.size
				return path, nil
			})
			if e != nil {
				return 1, e
			}
			op.LocalPath = path
			logf(os.Stdout, logInfo, "Source: %s · %s\n", cleanText(path), displayBytes(op.Size))
		case "forward":
			spec := normalizeForward(op.Port, op.RemoteHost, op.RemotePort)
			logf(os.Stdout, logInfo, "Forward %s\n", describeForward(spec.Port, spec.RemoteHost, spec.RemotePort, op.Reverse))
		}
	}
	if !directUpload {
		answer, err := input.Read("Type yes to approve: ", false)
		if err != nil {
			return 1, fmt.Errorf("approval cancelled; request remains pending: %w", err)
		}
		if answer != "yes" {
			return 1, fmt.Errorf("not approved; request remains pending")
		}
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
	if err = consumeApproval(dir, r.ID); err != nil {
		return 1, err
	}
	return runClient(options{host: r.Host, session: r.Session, server: r.Server, downloadDir: r.DownloadDir, ssh: r.SSH, quiet: r.Quiet, noEscape: r.NoEscape, approved: r.Operations, stay: a.stay, resumePID: r.PID, approvalID: r.ID})
}

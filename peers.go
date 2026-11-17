package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pkg/sftp"

	sshio "github.com/jay-waves/sshore/internal/ssh"
)

// Peer task records are advisory, never authorization. They contain only one
// connection's public resource ID, manager label and remote-facing description;
// local mount paths, control addresses/tokens and unrelated hosts are excluded.
// Server file mtime is the lease clock, avoiding client/server clock skew.
const remoteTaskRelativeDir = ".local/state/sshore/peer-tasks"
const remoteTaskRefresh = 20 * time.Second
const remoteTaskFresh = 75 * time.Second
const maxRemoteTaskSize = 8192

type peerRecord struct {
	Direction string `json:"direction,omitempty"`
	UID       string `json:"uid,omitempty"`
	Version   string `json:"version"`
	Key       string `json:"key"`
	ID        string `json:"id"`
	Manager   string `json:"manager"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
}

func taskManagerLabel() string {
	h, e := os.Hostname()
	if e != nil || h == "" {
		return "client"
	}
	h = cleanText(h)
	if len(h) > 128 {
		h = h[:128]
	}
	return h
}
func validateRemoteTask(t peerRecord) error {
	if t.UID != "" && !validTaskUID(t.UID) || t.Direction != "" && t.Direction != "<-" && t.Direction != "->" {
		return fmt.Errorf("invalid peer identity or direction")
	}
	if t.Version != version || !validRequestID(t.Key) || t.Manager == "" || len(t.Manager) > 128 || len(t.Detail) > 4096 {
		return fmt.Errorf("invalid peer task metadata")
	}
	kind, name, ok := strings.Cut(t.ID, "/")
	if !ok {
		return fmt.Errorf("invalid peer resource ID")
	}
	switch kind {
	case "mount", "forward":
		if validSession(name) != nil || t.State != "active" {
			return fmt.Errorf("invalid peer resource")
		}
	case "review":
		if validSession(name) != nil || t.State != "pending" {
			return fmt.Errorf("invalid pending task")
		}
	default:
		return fmt.Errorf("unsupported peer resource")
	}
	if strings.ContainsAny(t.Manager+t.Detail, "\x00\r\n\x1b") {
		return fmt.Errorf("invalid peer display text")
	}
	data, e := json.Marshal(t)
	if e != nil || len(data) > maxRemoteTaskSize {
		return fmt.Errorf("peer task metadata is too large")
	}
	return nil
}
func peerTaskDir(create bool) (string, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	dir := filepath.Join(home, filepath.FromSlash(remoteTaskRelativeDir))
	if create {
		if e = os.MkdirAll(dir, 0700); e != nil {
			return "", e
		}
	}
	return dir, privateStatePath(dir, true)
}
func storeRemoteTask(t peerRecord) error {
	if e := validateRemoteTask(t); e != nil {
		return e
	}
	dir, e := peerTaskDir(true)
	if e != nil {
		return e
	}
	return writeStateJSON(filepath.Join(dir, t.Key+".json"), t)
}
func removeRemoteTask(key string) error {
	if !validRequestID(key) {
		return fmt.Errorf("invalid peer task key")
	}
	dir, e := peerTaskDir(false)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	e = os.Remove(filepath.Join(dir, key+".json"))
	if os.IsNotExist(e) {
		return nil
	}
	return e
}
func listRemoteTasks() error { return printRemoteTasks(os.Stdout) }

type peerRow struct {
	task  peerRecord
	state string
}

func collectPeerRows() ([]peerRow, error) {
	dir, e := peerTaskDir(false)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	d, e := os.Open(dir)
	if e != nil {
		return nil, e
	}
	defer d.Close()
	names, e := d.Readdirnames(513)
	if e != nil && e != io.EOF {
		return nil, e
	}
	if len(names) > 512 {
		return nil, fmt.Errorf("peer task directory exceeds 512 records")
	}
	var rows []peerRow
	for _, name := range names {
		key := strings.TrimSuffix(name, ".json")
		if !strings.HasSuffix(name, ".json") || !validRequestID(key) {
			continue
		}
		p := filepath.Join(dir, name)
		if privateStatePath(p, false) != nil {
			continue
		}
		f, e := os.Open(p)
		if e != nil {
			continue
		}
		info, e := f.Stat()
		if e != nil {
			f.Close()
			continue
		}
		age := time.Since(info.ModTime())
		if age > 24*time.Hour {
			f.Close()
			removeExpiredPeerTask(p, info)
			continue
		}
		data, e := io.ReadAll(io.LimitReader(f, maxRemoteTaskSize+1))
		f.Close()
		if e != nil || len(data) > maxRemoteTaskSize {
			continue
		}
		var t peerRecord
		if json.Unmarshal(data, &t) != nil || validateRemoteTask(t) != nil || t.Key != key {
			continue
		}
		ttl := 24 * time.Hour
		if t.State == "pending" {
			ttl = requestTTL
		}
		if age > ttl {
			removeExpiredPeerTask(p, info)
			continue
		}
		state := t.State
		if age > remoteTaskFresh || age < -time.Minute {
			state = "unknown"
		}
		rows = append(rows, peerRow{t, state})
	}
	return rows, nil
}
func printRemoteTasks(output io.Writer) error {
	rows, e := collectPeerRows()
	if e != nil {
		return e
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].task, rows[j].task
		if a.Manager != b.Manager {
			return a.Manager < b.Manager
		}
		return a.ID < b.ID
	})
	fmt.Fprintln(output, "\nPeer-managed tasks (read-only; unknown = last known state):")
	out := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	defer out.Flush()
	fmt.Fprintln(out, "ID\tMANAGED BY\tSTATE\tDETAILS")
	for _, r := range rows {
		t := r.task
		fmt.Fprintf(out, "peer/%s/%s\t%s\t%s\t%s\n", t.Key[:8], t.ID, cleanText(t.Manager), r.state, cleanText(t.Detail))
	}
	return nil
}
func pendingRemoteTask(id, name string, ops []operation) peerRecord {
	detail := fmt.Sprintf("%d requested operation(s)", len(ops))
	if len(ops) == 1 {
		op := ops[0]
		if op.Kind == "forward" {
			detail = strings.ReplaceAll(requestSummary([]operation{op}), "your computer", "manager")
		} else {
			detail = op.Kind + " " + op.Path
		}
	}
	return peerRecord{Version: version, Key: id, ID: "review/" + name, Manager: taskManagerLabel(), State: "pending", Detail: cleanText(detail)}
}

func removeExpiredPeerTask(p string, previous os.FileInfo) {
	// Do not intentionally remove a newer heartbeat published by atomic rename.
	current, e := os.Lstat(p)
	if e == nil && os.SameFile(previous, current) {
		_ = os.Remove(p)
	}
}

func collectPeerViews() ([]peerView, error) {
	rows, e := collectPeerRows()
	if e != nil {
		return nil, e
	}
	out := []peerView{}
	for _, r := range rows {
		t := r.task
		out = append(out, peerView{Direction: t.Direction, UID: t.UID, Identity: t.Key, ID: "peer/" + t.Key[:8] + "/" + t.ID, Manager: t.Manager, State: r.state, Detail: t.Detail})
	}
	return out, nil
}

// Rejection has no live session transport. Remove only its advisory remote
// record, best effort, with existing authentication and a short timeout. Never
// prompt or change known_hosts; an unreachable mirror expires independently.
func removeRejectedPeerTask(r requestRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	server := r.Connection.Server
	if server == "" {
		server = "sshore"
	}
	command := "unset SSHORE_CONFIRM_ADDR SSHORE_CONFIRM_TOKEN; server=" + shellQuote(server) + "; if command -v \"$server\" >/dev/null 2>&1; then server=$(command -v \"$server\"); "
	if server == "sshore" {
		command += "else server=\"$HOME/.local/bin/sshore\"; "
	} else {
		command += "else :; "
	}
	command += "fi; if [ \"$(\"$server\" --version 2>/dev/null)\" = " + shellQuote("sshore "+version) + " ]; then exec \"$server\" task-remove " + shellQuote(version) + " " + shellQuote(r.ID) + "; fi"
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=2", "-o", "ConnectionAttempts=1", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-o", "ClearAllForwardings=yes", "-o", "ForkAfterAuthentication=no", "-o", "RemoteCommand=none", "-o", "SessionType=default"}
	args = append(args, r.Connection.SSH...)
	_ = sshio.Run(ctx, sshio.Options{Host: r.Connection.Host, Args: args, Command: command, Directory: r.Connection.Directory}, nil, io.Discard, io.Discard)
}

// Mount inventory uses the existing SFTP transport. Failure to publish metadata
// never fails file IO. Shutdown cleanup is bounded; a stalled server leaves an
// expiring advisory record rather than holding unmount hostage.

func publishSFTPTask(c *sftp.Client, dir string, t peerRecord) error {
	if e := validateRemoteTask(t); e != nil {
		return e
	}
	if _, e := c.Lstat(dir); os.IsNotExist(e) {
		if e = c.MkdirAll(dir); e != nil {
			return e
		}
		if e = c.Chmod(dir, 0700); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	info, e := c.Lstat(dir)
	if e != nil {
		return e
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("peer task directory is not private")
	}
	data, e := json.Marshal(t)
	if e != nil {
		return e
	}
	temp := path.Join(dir, ".task-"+t.Key)
	f, e := c.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return e
	}
	defer c.Remove(temp)
	if e = c.Chmod(temp, 0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	final := path.Join(dir, t.Key+".json")
	if _, ok := c.HasExtension("posix-rename@openssh.com"); ok {
		return c.PosixRename(temp, final)
	}
	_ = c.Remove(final)
	return c.Rename(temp, final)
}
func startMountTaskSync(parent context.Context, c *sftp.Client, task func() peerRecord) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		home, e := c.RealPath(".")
		if e != nil {
			return
		}
		dir := path.Join(home, remoteTaskRelativeDir)
		defer c.Remove(path.Join(dir, task().Key+".json"))
		ticker := time.NewTicker(remoteTaskRefresh)
		defer ticker.Stop()
		for {
			_ = publishSFTPTask(c, dir, task())
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// A locked record belongs to a live local transport helper. Peer heartbeat files
// alone never establish that this machine still participates in the resource.
func beginIncomingTask(t peerRecord) func() {
	noop := func() {}
	if validateRemoteTask(t) != nil || t.State != "active" {
		return noop
	}
	dir, err := stateDir("incoming")
	if err != nil {
		return noop
	}
	key := resourceToken()[:32]
	filename := filepath.Join(dir, key+".json")
	temporary := filepath.Join(dir, "."+key+".tmp")
	f, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return noop
	}
	if err = lockResourceState(f); err == nil {
		err = json.NewEncoder(f).Encode(t)
	}
	// Publish only after the lock and complete record are visible on the same inode.
	if err == nil {
		err = os.Rename(temporary, filename)
	}
	if err != nil {
		f.Close()
		os.Remove(temporary)
		return noop
	}
	return func() { os.Remove(filename); f.Close() }
}

func collectIncomingTasks() ([]peerView, error) {
	dir, err := stateDir("incoming")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	byKey := map[string]peerView{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		filename := filepath.Join(dir, entry.Name())
		if privateStatePath(filename, false) != nil {
			continue
		}
		f, err := os.OpenFile(filename, os.O_RDWR, 0600)
		if err != nil {
			continue
		}
		lockErr := lockResourceReadState(f)
		if lockErr == nil {
			// Unique connection filenames have no successor that could inherit the inode.
			old, _ := f.Stat()
			now, err := os.Lstat(filename)
			if err == nil && old != nil && os.SameFile(old, now) {
				os.Remove(filename)
			}
			f.Close()
			continue
		}
		if !resourceLockBusy(lockErr) {
			f.Close()
			continue
		}
		var t peerRecord
		err = json.NewDecoder(io.LimitReader(f, maxRemoteTaskSize)).Decode(&t)
		f.Close()
		if err != nil || validateRemoteTask(t) != nil || t.State != "active" {
			continue
		}
		// The leased peer record may supply a newer alias; the helper lock supplies liveness.
		if peerDir, err := peerTaskDir(false); err == nil {
			if data, err := os.ReadFile(filepath.Join(peerDir, t.Key+".json")); err == nil && len(data) <= maxRemoteTaskSize {
				var newer peerRecord
				if json.Unmarshal(data, &newer) == nil && validateRemoteTask(newer) == nil && newer.Key == t.Key && newer.UID == t.UID && newer.State == "active" {
					t = newer
				}
			}
		}
		byKey[t.Key] = peerView{Direction: t.Direction, UID: t.UID, Identity: t.Key, ID: t.ID, Manager: t.Manager, State: "active", Detail: t.Detail}
	}
	result := make([]peerView, 0, len(byKey))
	for _, v := range byKey {
		result = append(result, v)
	}
	return result, nil
}

func watchPeerRecords(ctx context.Context, input io.Reader, kind string, report func(error)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	updates := make(chan peerRecord)
	failure := make(chan error, 1)
	go func() {
		s := bufio.NewScanner(input)
		s.Buffer(make([]byte, 1024), maxRemoteTaskSize)
		for s.Scan() {
			var t peerRecord
			if e := json.Unmarshal(s.Bytes(), &t); e != nil {
				failure <- e
				return
			}
			select {
			case updates <- t:
			case <-ctx.Done():
				return
			}
		}
		failure <- s.Err()
	}()
	var first *peerRecord
	warned := false
	var endIncoming func()
	defer func() {
		if endIncoming != nil {
			endIncoming()
		}
		if first != nil {
			_ = removeRemoteTask(first.Key)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-failure:
			return e
		case t := <-updates:
			if e := validateRemoteTask(t); e != nil {
				return e
			}
			if t.State != "active" || (kind != "" && peerKind(t.ID) != kind) {
				return fmt.Errorf("watch requires an active resource")
			}
			if first != nil && (t.Key != first.Key || t.UID != first.UID || peerKind(t.ID) != peerKind(first.ID) || t.Manager != first.Manager || t.Direction != first.Direction) {
				return fmt.Errorf("resource identity changed during watch")
			}
			if first == nil {
				copy := t
				first = &copy
				endIncoming = beginIncomingTask(t)
			}
			if e := storeRemoteTask(t); e != nil && !warned {
				warned = true
				if report != nil {
					report(e)
				}
			}
		}
	}
}

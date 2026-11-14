package main

// Peer task records are advisory, never authorization. They contain only one
// connection's public resource ID, manager label and remote-facing description;
// local mount paths, control addresses/tokens and unrelated hosts are excluded.
// Server file mtime is the lease clock, avoiding client/server clock skew.
import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

const remoteTaskRelativeDir = ".local/state/sshore/peer-tasks"
const remoteTaskRefresh = 20 * time.Second
const remoteTaskFresh = 75 * time.Second
const maxRemoteTaskSize = 8192

type remoteTask struct {
	Version string `json:"version"`
	Key     string `json:"key"`
	ID      string `json:"id"`
	Manager string `json:"manager"`
	State   string `json:"state"`
	Detail  string `json:"detail"`
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
func validateRemoteTask(t remoteTask) error {
	if t.Version != version || !validApprovalID(t.Key) || t.Manager == "" || len(t.Manager) > 128 || len(t.Detail) > 4096 {
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
	case "approval":
		if !validApprovalID(name) || name != t.Key || t.State != "pending" {
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
	return dir, privateState(dir, true)
}
func storeRemoteTask(t remoteTask) error {
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
	if !validApprovalID(key) {
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
	task  remoteTask
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
		if !strings.HasSuffix(name, ".json") || !validApprovalID(key) {
			continue
		}
		p := filepath.Join(dir, name)
		if privateState(p, false) != nil {
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
		var t remoteTask
		if json.Unmarshal(data, &t) != nil || validateRemoteTask(t) != nil || t.Key != key {
			continue
		}
		ttl := 24 * time.Hour
		if t.State == "pending" {
			ttl = approvalTTL
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
func pendingRemoteTask(id string, ops []operation) remoteTask {
	detail := fmt.Sprintf("%d requested operation(s)", len(ops))
	if len(ops) == 1 {
		op := ops[0]
		if op.Kind == "forward" {
			detail = strings.ReplaceAll(approvalSummary([]operation{op}), "your computer", "manager")
		} else {
			detail = op.Kind + " " + op.Path
		}
	}
	return remoteTask{Version: version, Key: id, ID: "approval/" + id, Manager: taskManagerLabel(), State: "pending", Detail: cleanText(detail)}
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
		out = append(out, peerView{ID: "peer/" + t.Key[:8] + "/" + t.ID, Manager: t.Manager, State: r.state, Detail: t.Detail})
	}
	return out, nil
}

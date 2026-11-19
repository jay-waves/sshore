package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
)

// Peer advertisements are advisory; only daemon socket leases establish activity.
const remoteTaskFresh = daemonHeartbeatTimeout
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
func storeRemoteTask(t peerRecord) error {
	_, err := activityRPC(activityRequest{Kind: "peer_put", Peer: &t})
	return err
}
func removeRemoteTask(key string) error {
	_, err := activityRPC(activityRequest{Kind: "peer_remove", Key: key})
	return err
}
func listRemoteTasks() error { return printRemoteTasks(os.Stdout) }

func printRemoteTasks(output io.Writer) error {
	rows, e := collectPeerViews()
	if e != nil {
		return e
	}
	if len(rows) == 0 {
		return nil
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Manager != b.Manager {
			return a.Manager < b.Manager
		}
		return peerResourceID(a.ID) < peerResourceID(b.ID)
	})
	fmt.Fprintln(output, "\nPeer-managed tasks (read-only; unknown = last known state):")
	out := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	defer out.Flush()
	fmt.Fprintln(out, "ID\tMANAGED BY\tSTATE\tDETAILS")
	for _, row := range rows {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", row.ID, cleanText(row.Manager), row.State, cleanText(row.Detail))
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

func collectPeerViews() ([]peerView, error) {
	reply, err := activityRPC(activityRequest{Kind: "list"})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rows := make([]peerView, 0, len(reply.Peers))
	for _, entry := range reply.Peers {
		t := entry.Record
		row := t.view()
		row.ID = "peer/" + t.Key[:8] + "/" + t.ID
		if now.Sub(entry.Seen) > remoteTaskFresh {
			row.State = "unknown"
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Identity < rows[j].Identity })
	return rows, nil
}

func (t peerRecord) view() peerView {
	return peerView{Direction: t.Direction, UID: t.UID, Identity: t.Key, ID: t.ID, Manager: t.Manager, State: t.State, Detail: t.Detail}
}

// Rejection has no live session transport. Remove only its advisory remote
// record, best effort, with existing authentication and a short timeout. Never
// prompt or change known_hosts; an unreachable mirror expires independently.
func removeRejectedPeerTask(r requestRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := remoteHelperCommand(r.Connection.Host, r.Connection.Server, "task-remove", "", r.ID)
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=2", "-o", "ConnectionAttempts=1", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-o", "ClearAllForwardings=yes", "-o", "ForkAfterAuthentication=no", "-o", "RemoteCommand=none", "-o", "SessionType=default"}
	args = append(args, r.Connection.SSH...)
	_ = sshio.Run(ctx, sshio.Options{Host: r.Connection.Host, Args: args, Command: command, Directory: r.Connection.Directory}, nil, io.Discard, io.Discard)
}

func beginIncomingTask(t peerRecord) func() {
	stop, err := leaseActivity(activityRequest{Kind: "incoming", Peer: &t})
	if err != nil {
		return func() {}
	}
	return stop
}

func collectIncomingTasks() ([]peerView, error) {
	reply, err := activityRPC(activityRequest{Kind: "incoming"})
	if err != nil {
		return nil, err
	}
	result := make([]peerView, 0, len(reply.Peers))
	for _, entry := range reply.Peers {
		result = append(result, entry.Record.view())
	}
	return result, nil
}

func watchPeerRecords(input io.Reader, kind string, report func(error)) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 1024), maxRemoteTaskSize)
	var first peerRecord
	warned := false
	for scanner.Scan() {
		var t peerRecord
		if err := json.Unmarshal(scanner.Bytes(), &t); err != nil {
			return err
		}
		if err := validateRemoteTask(t); err != nil {
			return err
		}
		if t.State != "active" || (kind != "" && peerKind(t.ID) != kind) {
			return fmt.Errorf("watch requires an active resource")
		}
		if first.Key == "" {
			first = t
			defer removeRemoteTask(t.Key)
			defer beginIncomingTask(t)()
		} else if t.Key != first.Key || t.UID != first.UID || peerKind(t.ID) != peerKind(first.ID) || t.Manager != first.Manager || t.Direction != first.Direction {
			return fmt.Errorf("resource identity changed during watch")
		}
		if err := storeRemoteTask(t); err != nil && !warned {
			warned = true
			if report != nil {
				report(err)
			}
		}
	}
	return scanner.Err()
}

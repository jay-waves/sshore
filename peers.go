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

type peerRow struct {
	task  peerRecord
	state string
}

func collectPeerRows() ([]peerRow, error) {
	reply, err := activityRPC(activityRequest{Kind: "list"})
	if err != nil {
		return nil, err
	}
	var rows []peerRow
	for _, entry := range reply.Peers {
		state := entry.Record.State
		if time.Since(entry.Seen) > remoteTaskFresh {
			state = "unknown"
		}
		rows = append(rows, peerRow{task: entry.Record, state: state})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].task.Key < rows[j].task.Key })
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

func beginIncomingTask(t peerRecord) func() {
	stop, err := leaseActivity(activityRequest{Kind: "incoming", Peer: &t}, nil)
	if err != nil {
		return func() {}
	}
	return stop
}

func collectIncomingTasks() ([]peerView, error) {
	reply, err := activityRPC(activityRequest{Kind: "list"})
	if err != nil {
		return nil, err
	}
	byKey := map[string]peerView{}
	for _, t := range reply.Incoming {
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

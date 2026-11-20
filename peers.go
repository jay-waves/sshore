package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
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

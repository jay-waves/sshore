package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Peer advertisements are advisory; only daemon socket leases establish activity.
const maxRemoteTaskSize = 8192

type peerRecord struct {
	Direction string `json:"direction,omitempty"`
	UID       string `json:"uid"`
	Version   string `json:"version"`
	Key       string `json:"key"`
	ID        string `json:"id"`
	Manager   string `json:"manager"`
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
	if !validTaskUID(t.UID) || t.Direction != "<-" && t.Direction != "->" {
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
		if validSession(name) != nil {
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
func peerViews(peers []peerRecord) []peerView {
	rows := make([]peerView, 0, len(peers))
	for _, t := range peers {
		row := t.view()
		row.ID = "peer/" + t.Key[:8] + "/" + t.ID
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Identity < rows[j].Identity })
	return rows
}

func (t peerRecord) view() peerView {
	return peerView{Direction: t.Direction, UID: t.UID, Identity: t.Key, ID: t.ID, Manager: t.Manager, State: taskActive, Detail: t.Detail}
}

func beginIncomingTask(t peerRecord) func() {
	stop, err := leaseActivity(leaseRequest{Kind: "incoming", Peer: &t})
	if err != nil {
		return func() {}
	}
	return stop
}

func watchPeerRecords(input io.Reader, kind string, report func(error)) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 1024), maxRemoteTaskSize)
	var last peerRecord
	var stop func()
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	for scanner.Scan() {
		var t peerRecord
		if err := json.Unmarshal(scanner.Bytes(), &t); err != nil {
			return err
		}
		if err := validateRemoteTask(t); err != nil {
			return err
		}
		if kind != "" && peerKind(t.ID) != kind {
			return fmt.Errorf("watch requires an active %s", kind)
		}
		if last.Key == "" {
			last = t
		} else if t.Key != last.Key || t.UID != last.UID || peerKind(t.ID) != peerKind(last.ID) || t.Manager != last.Manager || t.Direction != last.Direction {
			return fmt.Errorf("resource identity changed during watch")
		}
		if stop != nil && t == last {
			continue
		}
		next, err := leaseActivity(leaseRequest{Kind: "incoming", Peer: &t})
		if err != nil {
			if report != nil {
				report(err)
				report = nil
			}
			continue
		}
		if stop != nil {
			stop()
		}
		stop, last = next, t
	}
	return scanner.Err()
}

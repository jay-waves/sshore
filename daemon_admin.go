package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"
)

func requestDaemonShutdown(expected daemonSnapshot) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := daemonAdmin(ctx, "shutdown", &expected); err != nil {
		return err
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		dir, err := daemonLocation()
		if err != nil {
			return err
		}
		owner, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		if err := privateStatePath(owner.Name(), false); err != nil {
			owner.Close()
			return err
		}
		lockErr := lockResourceState(owner)
		owner.Close()
		if lockErr == nil {
			return nil
		}
		if !resourceLockBusy(lockErr) {
			return lockErr
		}
		// Locator removal precedes owner release; absence alone is not exit.
		r, err := readLocalRecord(filepath.Join(dir, "daemon.json"), "daemon")
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && r.identity() != expected.Instance {
			return fmt.Errorf("daemon changed during shutdown; review again")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("daemon did not release its account endpoint; update stopped")
		case <-tick.C:
		}
	}
}

// Administrative control can inspect or stop a different release, but cannot
// create tasks or open data streams. Normal control still requires exact versions.
type daemonTaskInfo struct{ ID, Name string }
type daemonSnapshot struct {
	Version, Instance string
	Tasks             []daemonTaskInfo
}
type daemonAdminReply struct {
	Snapshot daemonSnapshot
	Error    string `json:",omitempty"`
}

func (s *daemonTasks) snapshot(record localShellRecord) daemonSnapshot {
	s.prune(time.Now()) // caller holds the task table lock
	r := daemonSnapshot{Version: record.Version, Instance: record.identity()}
	for id, task := range s.records {
		r.Tasks = append(r.Tasks, daemonTaskInfo{ID: id, Name: task.taskName()})
	}
	sort.Slice(r.Tasks, func(i, j int) bool { return r.Tasks[i].ID < r.Tasks[j].ID })
	return r
}
func daemonAdmin(ctx context.Context, action string, expected *daemonSnapshot) (daemonSnapshot, error) {
	r, err := existingDaemonRecord()
	if err != nil {
		return daemonSnapshot{}, err
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", r.Address)
	if err != nil {
		return daemonSnapshot{}, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := writeMessage(c, message{Type: "daemon_admin", Action: action, Token: r.Token}); err != nil {
		return daemonSnapshot{}, err
	}
	if action == "shutdown" {
		if err := json.NewEncoder(c).Encode(expected); err != nil {
			return daemonSnapshot{}, err
		}
	}
	var reply daemonAdminReply
	if err := json.NewDecoder(c).Decode(&reply); err != nil {
		return reply.Snapshot, err
	}
	if reply.Error != "" {
		return reply.Snapshot, fmt.Errorf("%s", reply.Error)
	}
	return reply.Snapshot, nil
}
func (s *daemonTasks) shutdown(record localShellRecord, expected daemonSnapshot) error {
	s.mu.Lock()
	if !reflect.DeepEqual(s.snapshot(record), expected) {
		s.mu.Unlock()
		return fmt.Errorf("tasks changed; review again")
	}
	s.draining = true
	var stops []interface{ stopTask() error }
	for id, task := range s.records {
		if stoppable, ok := task.(interface{ stopTask() error }); ok {
			stops = append(stops, stoppable)
		} else if _, ok := task.(requestRecord); ok {
			delete(s.records, id)
		}
	}
	s.mu.Unlock()
	results := make(chan error, len(stops))
	for _, task := range stops {
		go func() { results <- task.stopTask() }()
	}
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	var failures []error
	for range stops {
		select {
		case err := <-results:
			if err != nil {
				failures = append(failures, err)
			}
		case <-timer.C:
			return fmt.Errorf("daemon tasks did not exit; no forced termination performed")
		}
	}
	return errors.Join(failures...)
}
func (t *nativeShellTask) stopTask() error { t.shell.close(); return nil }
func (t *daemonResourceTask) stopTask() error {
	r := t.control("cancel")
	if r.Error != "" {
		return fmt.Errorf("%s", r.Error)
	}
	select {
	case <-t.done:
		return nil
	case <-time.After(20 * time.Second):
		return fmt.Errorf("resource did not exit")
	}
}

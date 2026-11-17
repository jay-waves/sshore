package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

func nativeShellRecords() ([]localShellRecord, error) {
	r, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := dialLocalSession(r)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err = writeMessage(c, message{Type: "task_list_native"}); err != nil {
		return nil, err
	}
	var records []localShellRecord
	err = json.NewDecoder(c).Decode(&records)
	return records, err
}

func (s *daemonTasks) nativeRecords() []localShellRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	var records []localShellRecord
	for _, task := range s.records {
		if task, ok := task.(*nativeShellTask); ok {
			task.shell.mu.Lock()
			records = append(records, task.shell.record)
			task.shell.mu.Unlock()
		}
	}
	return records
}

// The daemon owns task identities and their live implementations. Entries are
// never reconstructed after a daemon restart.
type daemonTask interface {
	taskName() string
	expired(time.Time) bool
}

type daemonTasks struct {
	draining bool
	mu       sync.Mutex
	records  map[string]daemonTask
}

func (r requestRecord) taskName() string           { return r.Name }
func (r requestRecord) expired(now time.Time) bool { return now.Sub(r.Created) > requestTTL }

type nativeShellTask struct {
	shell *localShell
	serve func(net.Conn, message) error
}

func (t *nativeShellTask) taskName() string {
	t.shell.mu.Lock()
	defer t.shell.mu.Unlock()
	return t.shell.record.Name
}
func (t *nativeShellTask) expired(time.Time) bool {
	select {
	case <-t.shell.done:
		return true
	default:
		return false
	}
}

func (s *daemonTasks) register(id string, task daemonTask) error {
	if !validTaskUID(id) {
		return fmt.Errorf("invalid task ID")
	}
	name := task.taskName()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return fmt.Errorf("daemon is shutting down")
	}
	s.prune(time.Now())
	if s.records == nil {
		s.records = make(map[string]daemonTask)
	}
	if _, exists := s.records[id]; exists {
		return fmt.Errorf("task already exists")
	}
	for _, old := range s.records {
		if old.taskName() == name {
			return fmt.Errorf("%s: %w", name, errObjectNameInUse)
		}
	}
	s.records[id] = task
	return nil
}

func (s *daemonTasks) open(c net.Conn, id string) {
	s.mu.Lock()
	s.prune(time.Now())
	task, ok := s.records[id].(interface{ serveTask(net.Conn, message) error })
	s.mu.Unlock()
	if !ok {
		_ = writeMessage(c, message{Type: "error", Error: "task no longer exists"})
		return
	}
	if writeMessage(c, message{Type: "task_ready"}) != nil {
		return
	}
	m, err := readMessage(c)
	if err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	_ = task.serveTask(c, m)
}

func (t *nativeShellTask) serveTask(c net.Conn, m message) error { return t.serve(c, m) }

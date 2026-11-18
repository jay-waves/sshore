package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
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
	var records []localShellRecord
	err = daemonJSON(context.Background(), r, "/tasks/native", nil, &records)
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

var errDaemonDraining = errors.New("daemon is shutting down")

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
		return errDaemonDraining
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

func (s *daemonTasks) open(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	s.prune(time.Now())
	task, ok := s.records[id].(interface{ serveTask(net.Conn, message) error })
	s.mu.Unlock()
	if !ok {
		http.Error(w, "task no longer exists", 404)
		return
	}
	serveDaemonStream(w, r, func(c net.Conn) {
		m, err := readMessage(c)
		if err != nil {
			return
		}
		_ = c.SetDeadline(time.Time{})
		_ = task.serveTask(c, m)
	})
}

func (t *nativeShellTask) serveTask(c net.Conn, m message) error { return t.serve(c, m) }

type resourceReply struct {
	Status  resourceStatus
	Records []resourceRecord `json:",omitempty"`
	Error   string           `json:",omitempty"`
}

// A mount owns a child process; a forward owns an in-process executor. Both
// expose the same bounded control operations through the account task table.
type daemonResourceTask struct {
	mu      sync.Mutex
	record  resourceRecord
	done    <-chan struct{}
	control func(string) resourceReply
}

func (t *daemonResourceTask) taskName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.record.Name
}
func (t *daemonResourceTask) expired(time.Time) bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}
func (s *daemonTasks) resourceControl(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.prune(time.Now())
	t, ok := s.records[r.PathValue("id")].(*daemonResourceTask)
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	var reply resourceReply
	switch r.PathValue("operation") {
	case "kill":
		reply = t.stop()
	case "status":
		reply = t.control("status")
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(reply)
}
func (t *daemonResourceTask) stop() resourceReply {
	reply := t.control("kill")
	if reply.Error == "" {
		select {
		case <-t.done:
		case <-time.After(20 * time.Second):
			reply.Error = "resource cleanup is still pending"
		}
	}
	return reply
}

func (t *daemonResourceTask) publish(r resourceRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r.Name = t.record.Name
	t.record = r
}

func (s *daemonTasks) resourceRecords() []resourceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	var records []resourceRecord
	for _, task := range s.records {
		if t, ok := task.(*daemonResourceTask); ok {
			t.mu.Lock()
			records = append(records, t.record)
			t.mu.Unlock()
		}
	}
	return records
}
func (s *daemonTasks) renameResource(id, name string) error {
	if err := validAlias(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	t, ok := s.records[id].(*daemonResourceTask)
	if !ok {
		return os.ErrNotExist
	}
	for key, other := range s.records {
		if key != id && other.taskName() == name {
			return fmt.Errorf("%s: %w", name, errObjectNameInUse)
		}
	}
	t.mu.Lock()
	t.record.Name = name
	t.mu.Unlock()
	return nil
}

func daemonResourceRequest(ctx context.Context, path string, input any) (resourceReply, error) {
	var reply resourceReply
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) && path == "/resources" {
		return reply, nil
	}
	if err == nil {
		err = daemonJSON(ctx, record, path, input, &reply)
	}
	if err == nil && reply.Error != "" {
		err = fmt.Errorf("%s", reply.Error)
	}
	return reply, err
}

func (s *resourceService) control(action string) resourceReply {
	if action == "status" {
		return resourceReply{Status: s.statusSnapshot()}
	}
	if action != "kill" {
		return resourceReply{Error: "invalid resource operation"}
	}
	request := make(chan error, 1)
	select {
	case s.stop <- request:
	case <-s.done:
		return resourceReply{}
	}
	select {
	case err := <-request:
		if err != nil {
			return resourceReply{Error: err.Error()}
		}
	case <-s.done:
	}
	return resourceReply{}
}

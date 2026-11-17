package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

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
func (t *daemonResourceTask) serveTask(c net.Conn, m message) error {
	if m.Type != "status" && m.Type != "kill" {
		return fmt.Errorf("invalid resource operation")
	}
	reply := t.control(m.Type)
	if m.Type == "kill" && reply.Error == "" {
		select {
		case <-t.done:
		case <-time.After(20 * time.Second):
			reply.Error = "resource cleanup is still pending"
		}
	}
	return json.NewEncoder(c).Encode(reply)
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

func daemonResourceRequest(m message) (resourceReply, error) {
	return daemonResourceRequestContext(context.Background(), m)
}
func daemonResourceRequestContext(ctx context.Context, m message) (resourceReply, error) {
	r, err := existingDaemonRecord()
	if os.IsNotExist(err) && m.Type == "task_list_resources" {
		return resourceReply{}, nil
	}
	if err != nil {
		return resourceReply{}, err
	}
	if m.Type == "status" || m.Type == "kill" {
		r.UID = m.Instance
	}
	c, err := dialLocalSessionContext(ctx, r, time.Now().Add(5*time.Second))
	if err != nil {
		return resourceReply{}, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(25 * time.Second))
	if err := writeMessage(c, m); err != nil {
		return resourceReply{}, err
	}
	var reply resourceReply
	if err := json.NewDecoder(c).Decode(&reply); err != nil {
		return reply, err
	}
	if reply.Error != "" {
		return reply, fmt.Errorf("%s", reply.Error)
	}
	return reply, nil
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

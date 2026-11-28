package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/tmux"
)

// The account daemon reviews shells and resources together for shutdown.
type daemonTask interface {
	stopTask() error
	taskName() string
}

var errDaemonDraining = errors.New("daemon is shutting down")

type daemonTasks struct {
	publishInventory func(...string)
	log              io.Writer
	cgroup           string
	cgroupErr        error
	shells           tmux.Manager[*attachment, *tmuxTask]
	draining         bool
	mu               sync.Mutex
	resources        map[string]*daemonResourceTask
}

func (s *daemonTasks) inventoryChanged(ids ...string) {
	if s != nil && s.publishInventory != nil {
		s.publishInventory(ids...)
	}
}

func (s *daemonTasks) newShell(record localShellRecord, env []string, parent *tmuxTask, hosted bool) (*tmuxTask, error) {
	s.mu.Lock()
	uid := newTaskUID()
	name, err := s.admitLocked(uid, record.Name)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	record.UID, record.Name = uid, name
	var terminalParent *tmux.Task[*attachment, *tmuxTask]
	if parent != nil {
		terminalParent = parent.runtime
	}
	task := &tmuxTask{
		registry: s, log: s.log, cgroup: s.cgroup, cgroupErr: s.cgroupErr,
		record: record, environment: env, hosted: hosted,
	}
	task.runtime = s.shells.NewSession(uid, name, task, terminalParent, &task.mu, tmux.Events[*attachment, *tmuxTask]{
		Changed: s.inventoryChanged, Exit: task.reportTerminalExit,
		ParentEnded: task.reportParentEnd, StopFailed: task.reportReapFailure,
		AttachmentLost: task.reportAttachmentLoss,
	})
	err = s.shells.Register(task.runtime)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.inventoryChanged(uid)
	return task, nil
}

func (s *daemonTasks) shell(id string) *tmuxTask {
	task, _ := s.shells.Lookup(id)
	return task
}

// The account keeps one namespace and one admission lock for shells and resources.
func (s *daemonTasks) admitLocked(id, name string) (string, error) {
	if s.draining {
		return "", errDaemonDraining
	}
	if !validTaskUID(id) {
		return "", fmt.Errorf("invalid task ID")
	}
	if s.lookupLocked(id) != nil {
		return "", fmt.Errorf("task already exists")
	}
	if name == "" {
		name = s.automaticNameLocked(id)
		if name == "" {
			return "", fmt.Errorf("no available task name for %s", id)
		}
	} else {
		if err := validSession(name); err != nil {
			return "", err
		}
		for _, existing := range s.liveLocked() {
			if existing.taskName() == name {
				return "", fmt.Errorf("%s: %w", name, errObjectNameInUse)
			}
		}
	}
	return name, nil
}

// The caller holds the account admission lock.
func (s *daemonTasks) liveLocked() iter.Seq2[string, daemonTask] {
	return func(yield func(string, daemonTask) bool) {
		for _, task := range s.shells.Sessions() {
			if !yield(task.record.UID, task) {
				return
			}
		}
		for id, task := range s.resources {
			if !yield(id, task) {
				return
			}
		}
	}
}

func (s *daemonTasks) lookupLocked(id string) daemonTask {
	if task := s.shell(id); task != nil {
		return task
	}
	if task := s.resources[id]; task != nil {
		return task
	}
	return nil
}

func (s *daemonTasks) lookup(id string) daemonTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookupLocked(id)
}

func (s *daemonTasks) resourceTasks() []*daemonResourceTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*daemonResourceTask, 0, len(s.resources))
	for _, task := range s.resources {
		result = append(result, task)
	}
	return result
}

func (s *daemonTasks) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range s.liveLocked() {
		return true
	}
	return false
}

func (s *daemonTasks) automaticNameLocked(uid string) string {
	// An automatic name can only collide with prefixes of this UID.
	var taken [33]bool
	for _, task := range s.liveLocked() {
		name := task.taskName()
		if len(name) >= 6 && strings.HasPrefix(uid, name) {
			taken[len(name)] = true
		}
	}
	for length := 6; length <= len(uid); length++ {
		if !taken[length] {
			return uid[:length]
		}
	}
	return ""
}

func (s *daemonTasks) open(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 {
		http.Error(w, "task stream upgrade requires an empty request body", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	task := s.shell(id)
	if task == nil {
		http.Error(w, "task no longer exists", 404)
		return
	}
	serveDaemonStream(w, r, func(c net.Conn) { serveTaskCommand(c, task.serveTask) })
}

func (s *daemonTasks) openNative(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 {
		http.Error(w, "task stream upgrade requires an empty request body", http.StatusBadRequest)
		return
	}
	query := r.URL.Query()
	task, record, err := s.findNativeShell(query.Get("ref"), query.Get("identity"))
	reply := localStartup{Record: record}
	if err != nil {
		reply.Error = err.Error()
	}
	serveDaemonStream(w, r, func(c net.Conn) {
		if err := writeControlJSON(c, reply); err == nil && task != nil {
			serveTaskCommand(c, task.serveTask)
		}
	})
}

func serveTaskCommand(c net.Conn, serve func(net.Conn, message) error) {
	m, err := readMessage(c)
	if err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	_ = serve(c, m)
}

func (s *daemonTasks) findNativeShell(ref, identity string) (*tmuxTask, localShellRecord, error) {
	task := s.shell(canonicalTaskUID(identity))
	if task == nil || task.hosted {
		return nil, localShellRecord{}, nil
	}
	record := task.record
	if record.identity() != identity || ref != "" && ref != record.Name && ref != record.UID {
		return nil, localShellRecord{}, fmt.Errorf("local task changed; refresh with sshore list")
	}
	return task, record, nil
}

type resourceMutation struct {
	Kind, Ref, Identity string
}

func (change resourceMutation) confirmedUID() string {
	id, ok := strings.CutPrefix(change.Identity, "task:")
	if ok && validTaskUID(id) {
		return id
	}
	return ""
}

type resourceReply struct {
	Error    string   `json:",omitempty"`
	Failures []string `json:",omitempty"`
}

// A mount owns a child process; a forward owns an in-process executor. Both
// expose the same bounded control operations through the account task table.
type daemonResourceTask struct {
	mu     sync.Mutex
	record resourceRecord // Only Detail, Remote and Mountpoint change after registration.
	done   chan struct{}
	stopMu sync.Mutex
	stop   func() error // Cleared once the worker accepts the stop request.
}

func (s *daemonTasks) registerResource(record resourceRecord, cancel context.CancelFunc) (*daemonResourceTask, error) {
	if record.UID == "" {
		record.UID = newTaskUID()
	}
	s.mu.Lock()
	name, err := s.admitLocked(record.UID, record.Name)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	record.Name = name
	task := &daemonResourceTask{record: record, done: make(chan struct{}), stop: func() error {
		cancel()
		return nil
	}}
	if s.resources == nil {
		s.resources = make(map[string]*daemonResourceTask)
	}
	s.resources[record.UID] = task
	s.mu.Unlock()
	s.inventoryChanged(record.UID)
	return task, nil
}

func (t *daemonResourceTask) taskName() string {
	return t.record.Name
}
func (t *daemonResourceTask) expired() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

func (s *daemonTasks) serveResourcesForHost(w http.ResponseWriter, r *http.Request) {
	var host string
	if !readDaemonBody(w, r, &host) {
		return
	}
	var rows []resourceView
	for _, task := range s.resourceTasks() {
		if task.record.Host == host {
			task.mu.Lock()
			rows = append(rows, viewRecord(task.record))
			task.mu.Unlock()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, rows)
}

const resourceStopTimeout = 20 * time.Second

func (t *daemonResourceTask) requestStop() error {
	t.stopMu.Lock()
	defer t.stopMu.Unlock()
	if t.stop == nil || t.expired() {
		return nil
	}
	if err := t.stop(); err != nil {
		return err
	}
	t.stop = nil
	return nil
}

func (t *daemonResourceTask) stopTask() error {
	if err := t.requestStop(); err != nil {
		return err
	}
	timer := time.NewTimer(resourceStopTimeout)
	defer timer.Stop()
	select {
	case <-t.done:
		return nil
	case <-timer.C:
		return errors.New("resource cleanup is still pending")
	}
}

func (s *daemonTasks) finishResource(t *daemonResourceTask) {
	s.mu.Lock()
	delete(s.resources, t.record.UID)
	t.mu.Lock()
	close(t.done)
	t.mu.Unlock()
	s.mu.Unlock()
	s.inventoryChanged(t.record.UID)
}

// Startup supplies runtime details. The task table owns identity and naming.
func (s *daemonTasks) resourceReady(t *daemonResourceTask, detail, root, mountpoint string) resourceRecord {
	t.mu.Lock()
	t.record.Detail, t.record.Remote, t.record.Mountpoint = detail, root, mountpoint
	record := t.record
	t.mu.Unlock()
	s.inventoryChanged(t.record.UID)
	return record
}

func (s *daemonTasks) serveTaskStop(w http.ResponseWriter, r *http.Request) {
	var request struct{ UID string }
	if !readDaemonBody(w, r, &request) {
		return
	}
	var reply resourceReply
	if task := s.lookup(request.UID); task != nil {
		if err := task.stopTask(); err != nil {
			reply.Error = err.Error()
		}
	} else {
		reply.Error = "task changed; refresh with sshore list"
	}
	_ = json.MarshalWrite(w, reply)
}

func (s *daemonTasks) confirmedResourceLocked(change resourceMutation, id string) *daemonResourceTask {
	task := s.resources[id]
	if task != nil && task.matches(change) {
		return task
	}
	return nil
}

func (t *daemonResourceTask) matches(change resourceMutation) bool {
	return t.record.Kind == change.Kind && (t.record.Name == change.Ref || t.record.UID == change.Ref)
}

// Resolve all confirmed identities before stopping any workers.
func (s *daemonTasks) confirmedResourceStops(changes []resourceMutation) ([]*daemonResourceTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stops := make([]*daemonResourceTask, len(changes))
	selected := make(map[string]struct{}, len(changes))
	for i, change := range changes {
		id := change.confirmedUID()
		_, duplicate := selected[id]
		if id == "" || duplicate || validSession(change.Ref) != nil {
			return nil, fmt.Errorf("invalid confirmed resource selection")
		}
		switch change.Kind {
		case "mount", "forward":
			task := s.confirmedResourceLocked(change, id)
			if task == nil {
				return nil, fmt.Errorf("%s/%s changed; review again", change.Kind, change.Ref)
			}
			stops[i] = task
			selected[id] = struct{}{}
		default:
			return nil, fmt.Errorf("invalid confirmed resource type")
		}
	}
	return stops, nil
}

func (s *daemonTasks) serveConfirmedResourceStops(w http.ResponseWriter, r *http.Request) {
	var changes []resourceMutation
	if !readDaemonBody(w, r, &changes) {
		return
	}
	stops, err := s.confirmedResourceStops(changes)
	if err != nil {
		_ = json.MarshalWrite(w, resourceReply{Error: err.Error()})
		return
	}
	failures := make([]string, len(stops))
	// Each worker owns its result slot and can signal even if the caller leaves.
	finished := make(chan struct{}, len(stops))
	for i, task := range stops {
		go func() {
			if err := task.stopTask(); err != nil {
				failures[i] = err.Error()
			}
			finished <- struct{}{}
		}()
	}
	for range stops {
		select {
		case <-finished:
		case <-r.Context().Done():
			return
		}
	}
	_ = json.MarshalWrite(w, resourceReply{Failures: failures})
}

func daemonResourceRequest(ctx context.Context, path string, input any) (resourceReply, error) {
	var reply resourceReply
	record, err := existingDaemonRecord()
	if err == nil {
		err = daemonJSON(ctx, record, path, input, &reply)
	}
	if err == nil && reply.Error != "" {
		err = fmt.Errorf("%s", reply.Error)
	}
	return reply, err
}

var errObjectNameInUse = errors.New("name is already in use")

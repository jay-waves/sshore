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

// The daemon owns task identities and their live implementations. Entries are
// never reconstructed after a daemon restart.
type daemonTask interface {
	stopTask() error
	setInitialIdentity(uid, name string) // Called before publication in the registry.
	taskName() string
	expired() bool
}

var errDaemonDraining = errors.New("daemon is shutting down")

type daemonTasks struct {
	publishInventory func(...daemonTask)
	log              io.Writer
	cgroup           string
	cgroupErr        error
	forest           tmux.Forest[*tmuxTask]
	draining         bool
	mu               sync.Mutex
	records          map[string]daemonTask
}

func (s *daemonTasks) inventoryChanged(tasks ...daemonTask) {
	if s != nil && s.publishInventory != nil {
		s.publishInventory(tasks...)
	}
}

func (s *daemonTasks) newTmuxTask(record localShellRecord, env []string, parent *tmuxTask, hosted bool) *tmuxTask {
	var terminalParent *tmux.Task[*attachment, *tmuxTask]
	if parent != nil {
		terminalParent = &parent.runtime
	}
	return &tmuxTask{
		registry: s, runtime: tmux.NewTask[*attachment](&s.forest, terminalParent), log: s.log, cgroup: s.cgroup, cgroupErr: s.cgroupErr,
		record: record, environment: env, hosted: hosted,
	}
}

// Register both shell ingress capabilities against the same managed lifecycle.
func (s *daemonTasks) registerShell(task *tmuxTask, parent *tmuxTask) error {
	if err := s.register("", task); err != nil {
		return err
	}
	task.watchParent(parent)
	return nil
}

func (s *daemonTasks) childShells(parent *tmuxTask) []*tmuxTask {
	registered := liveTasks[*tmuxTask](s)
	var children []*tmuxTask
	for _, task := range registered {
		if child := task; child.parentTask() == parent {
			children = append(children, child)
		}
	}
	return children
}

func (s *daemonTasks) register(id string, task daemonTask) error {
	if err := s.registerTask(id, task); err != nil {
		return err
	}
	s.inventoryChanged(task)
	return nil
}

func (s *daemonTasks) registerTask(id string, task daemonTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return errDaemonDraining
	}
	if id == "" {
		id = newTaskUID()
	} else if !validTaskUID(id) {
		return fmt.Errorf("invalid task ID")
	}
	if s.lookupLocked(id) != nil {
		return fmt.Errorf("task already exists")
	}
	name := task.taskName()
	if name == "" {
		name = s.automaticNameLocked(id)
		if name == "" {
			return fmt.Errorf("no available task name for %s", id)
		}
	} else {
		if err := validSession(name); err != nil {
			return err
		}
		for _, existing := range s.liveLocked() {
			if existing.taskName() == name {
				return fmt.Errorf("%s: %w", name, errObjectNameInUse)
			}
		}
	}
	task.setInitialIdentity(id, name)
	if s.records == nil {
		s.records = make(map[string]daemonTask)
	}
	s.records[id] = task
	return nil
}

// The caller holds the task table lock. Scans remove finished entries.
func (s *daemonTasks) liveLocked() iter.Seq2[string, daemonTask] {
	return func(yield func(string, daemonTask) bool) {
		for id, task := range s.records {
			if task.expired() {
				delete(s.records, id)
				continue
			}
			if !yield(id, task) {
				return
			}
		}
	}
}

// The caller holds the task table lock. A targeted lookup need only retire
// the requested entry; global scans remain responsible for other entries.
func (s *daemonTasks) lookupLocked(id string) daemonTask {
	task := s.records[id]
	if task != nil && task.expired() {
		delete(s.records, id)
		return nil
	}
	return task
}

func (s *daemonTasks) lookup(id string) daemonTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookupLocked(id)
}

func liveTasks[T daemonTask](s *daemonTasks) []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []T
	for _, task := range s.liveLocked() {
		if t, ok := task.(T); ok {
			result = append(result, t)
		}
	}
	return result
}

func (s *daemonTasks) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return len(s.records) != 0
}

func (s *daemonTasks) pruneLocked() {
	for range s.liveLocked() {
	}
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
	task, ok := s.lookup(id).(interface{ serveTask(net.Conn, message) error })
	if !ok {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	query := taskQuery{Kind: "native", Ref: ref}
	if identity != "" {
		query.UID = canonicalTaskUID(identity)
		if query.UID == "" {
			return nil, localShellRecord{}, nil
		}
	}
	target, err := query.selectLocked(s)
	if err != nil || target.Identity == "" {
		return nil, localShellRecord{}, err
	}
	task, ok := s.records[canonicalTaskUID(target.Identity)].(*tmuxTask)
	if !ok || task.hosted {
		return nil, localShellRecord{}, fmt.Errorf("local task changed; refresh with sshore list")
	}
	record := task.record
	if identity != "" && (record.identity() != identity || ref != "" && ref != record.Name && ref != record.UID) {
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
	mu           sync.Mutex
	record       resourceRecord // Only Detail, Remote and Mountpoint change after registration.
	done         <-chan struct{}
	control      func(string) resourceReply
	stopMu       sync.Mutex
	stopAccepted bool // Protected by stopMu.
}

func (t *daemonResourceTask) setInitialIdentity(uid, name string) {
	t.record.UID, t.record.Name = uid, name
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
	for _, task := range liveTasks[*daemonResourceTask](s) {
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
	if t.stopAccepted || t.expired() {
		return nil
	}
	reply := t.control("kill")
	// A refusal leaves the task connected and may be retried.
	if reply.Error != "" {
		return errors.New(reply.Error)
	}
	t.stopAccepted = true
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

func (t *daemonResourceTask) finish(done chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	close(done)
}

// Startup supplies runtime details. The task table owns identity and naming.
func (t *daemonResourceTask) publishReady(detail, root, mountpoint string) resourceRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.record.Detail, t.record.Remote, t.record.Mountpoint = detail, root, mountpoint
	return t.record
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
	task, _ := s.lookupLocked(id).(*daemonResourceTask)
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

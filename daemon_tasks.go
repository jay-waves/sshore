package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
	log      io.Writer
	forest   shellForest
	draining bool
	mu       sync.Mutex
	records  map[string]daemonTask
}

type nativeShellTask struct {
	shell *localShell
	serve func(net.Conn, message) error
}

func (t *nativeShellTask) setInitialIdentity(uid, name string) { t.shell.setInitialIdentity(uid, name) }
func (t *nativeShellTask) taskName() string                    { return t.shell.taskName() }
func (t *nativeShellTask) expired() bool                       { return t.shell.expired() }
func (t *nativeShellTask) managedShell() *shellTask            { return &t.shell.shellTask }

func (s *daemonTasks) register(id string, task daemonTask) error {
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
		if err := s.checkNameLocked("", name); err != nil {
			return err
		}
	}
	task.setInitialIdentity(id, name)
	if s.records == nil {
		s.records = make(map[string]daemonTask)
	}
	s.records[id] = task
	return nil
}

func (s *daemonTasks) checkNameLocked(except, name string) error {
	for id, task := range s.liveLocked() {
		if id != except && task.taskName() == name {
			return fmt.Errorf("%s: %w", name, errObjectNameInUse)
		}
	}
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
		if err := json.NewEncoder(c).Encode(reply); err == nil && task != nil {
			serveTaskCommand(c, task.serve)
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

func (s *daemonTasks) findNativeShell(ref, identity string) (*nativeShellTask, localShellRecord, error) {
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
	task := s.records[canonicalTaskUID(target.Identity)].(*nativeShellTask)
	record := task.snapshot()
	if identity != "" && (record.identity() != identity || ref != "" && ref != record.Name && ref != record.UID) {
		return nil, localShellRecord{}, fmt.Errorf("local task changed; refresh with sshore list")
	}
	return task, record, nil
}

func (t *nativeShellTask) snapshot() localShellRecord {
	// Only the label changes after publication.
	t.shell.nameMu.Lock()
	defer t.shell.nameMu.Unlock()
	return t.shell.record
}

func (t *nativeShellTask) serveTask(c net.Conn, m message) error { return t.serve(c, m) }

type resourceMutation struct {
	Kind, Ref, Identity, NewName string
}

func (change resourceMutation) confirmedUID() string {
	id, ok := strings.CutPrefix(change.Identity, "task:")
	if ok && validTaskUID(id) {
		return id
	}
	return ""
}

type resourceReply struct {
	Status   resourceStatus
	Error    string   `json:",omitempty"`
	Failures []string `json:",omitempty"`
}

// A mount owns a child process; a forward owns an in-process executor. Both
// expose the same bounded control operations through the account task table.
type daemonResourceTask struct {
	mu            sync.Mutex
	record        resourceRecord // UID and Host are immutable after registration.
	done          <-chan struct{}
	control       func(string) resourceReply
	statusPending atomic.Bool
	stopMu        sync.Mutex
	stopAccepted  bool // Protected by stopMu.
}

func (t *daemonResourceTask) setInitialIdentity(uid, name string) {
	t.record.UID, t.record.Name = uid, name
}

func (t *daemonResourceTask) taskName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
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

const resourceStatusTimeout = 2 * time.Second

type resourceStatusRow struct {
	UID    string
	Status resourceStatus
}

func (s *daemonTasks) serveTaskInventory(w http.ResponseWriter, r *http.Request) {
	var request taskInventoryRequest
	if !readDaemonBody(w, r, &request) {
		return
	}
	if request.Detail != "" && request.Detail != "health" && request.Detail != "inventory" && request.Detail != "sessions" {
		http.Error(w, "invalid shell inventory detail", http.StatusBadRequest)
		return
	}
	snapshot, workers := s.taskInventory(request)
	enc := json.NewEncoder(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	if err := enc.Encode(snapshot); err != nil || len(workers) == 0 {
		return
	}
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), resourceStatusTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	_ = controller.SetWriteDeadline(deadline)
	probeResourceStatus(ctx, workers, func(row resourceStatusRow) bool {
		return enc.Encode(row) == nil && controller.Flush() == nil
	})
}

func probeResourceStatus(ctx context.Context, workers []*daemonResourceTask, emit func(resourceStatusRow) bool) {

	probes := len(workers)
	concurrency := min(8, probes)
	// An outstanding probe can always return, including after this request ends.
	rows := make(chan resourceStatusRow, concurrency)
	startNext := func() {
		if len(workers) == 0 || ctx.Err() != nil {
			return
		}
		task := workers[0]
		workers = workers[1:]
		go func() { rows <- task.readStatus() }()
	}
	for range concurrency {
		startNext()
	}

	// Flush each result so a slow worker cannot hide completed results when
	// the caller's aggregate deadline expires.
	for range probes {
		select {
		case row := <-rows:
			startNext()
			if row.UID != "" && !emit(row) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (task *daemonResourceTask) readStatus() resourceStatusRow {
	if task.expired() || !task.statusPending.CompareAndSwap(false, true) {
		return resourceStatusRow{}
	}
	// A blocked worker can retain one probe across concurrent inventories.
	defer task.statusPending.Store(false)
	reply := task.control("status")
	if reply.Error != "" {
		return resourceStatusRow{}
	}
	return resourceStatusRow{UID: task.record.UID, Status: reply.Status}
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

type hostedShellView interface {
	hostedView(string) (shellView, int)
}

type taskInventoryRequest struct {
	Native, Hosted, Resources, Status bool
	Host, Detail                      string
}

type taskInventorySnapshot struct {
	Native    []localShellView
	Shells    []shellView
	Resources []resourceRecord
}

func (s *daemonTasks) taskInventory(request taskInventoryRequest) (taskInventorySnapshot, []*daemonResourceTask) {
	var snapshot taskInventorySnapshot
	var groups []int
	var workers []*daemonResourceTask
	// Capture membership before collecting runtime state. A shell view may wait
	// for its terminal handshake, so it must not hold up unrelated task lookups.
	for _, task := range liveTasks[daemonTask](s) {
		switch t := task.(type) {
		case *nativeShellTask:
			if request.Native {
				view := t.shell.view()
				if request.Detail != "health" {
					view.Directory = currentShellDirectory(view.PID, view.Directory)
				}
				snapshot.Native = append(snapshot.Native, view)
			}
		case hostedShellView:
			if request.Hosted {
				view, group := t.hostedView(request.Detail)
				snapshot.Shells = append(snapshot.Shells, view)
				groups = append(groups, group)
			}
		case *daemonResourceTask:
			if !request.Resources || request.Host != "" && t.record.Host != request.Host {
				continue
			}
			t.mu.Lock()
			snapshot.Resources = append(snapshot.Resources, t.record)
			t.mu.Unlock()
			if request.Status {
				workers = append(workers, t)
			}

		}
	}
	hostedDiagnostics(snapshot.Shells, groups, request.Detail)
	return snapshot, workers
}

func (s *daemonTasks) serveResourceMutation(w http.ResponseWriter, r *http.Request) {
	var mutation resourceMutation
	if !readDaemonBody(w, r, &mutation) {
		return
	}
	task, err := s.mutateResource(r.PathValue("action"), mutation)
	if err == nil && task != nil {
		err = task.stopTask()
	}
	var reply resourceReply
	if err != nil {
		reply.Error = err.Error()
	}
	_ = json.NewEncoder(w).Encode(reply)
}

// A stop returns the exact registered worker to stop outside the task table
// lock. Renames complete under the same lock as lookup.
func (s *daemonTasks) mutateResource(action string, change resourceMutation) (*daemonResourceTask, error) {
	if action != "kill" && action != "rename" {
		return nil, fmt.Errorf("invalid resource operation")
	}
	if err := validSession(change.Ref); err != nil {
		return nil, err
	}
	if action == "rename" {
		if err := validAlias(change.NewName); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, task, err := s.resolveResourceLocked(change)
	if err != nil {
		return nil, err
	}

	if action == "kill" {
		return task, nil
	}
	if err := s.checkNameLocked(id, change.NewName); err != nil {
		return nil, err
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.expired() {
		return nil, fmt.Errorf("task is exiting; refresh with sshore list")
	}
	task.record.Name = change.NewName
	return nil, nil
}

func (s *daemonTasks) resolveResourceLocked(change resourceMutation) (string, *daemonResourceTask, error) {
	switch change.Kind {
	case "mount", "forward":
	default:
		return "", nil, fmt.Errorf("invalid resource type")
	}
	if change.Identity != "" {
		id := change.confirmedUID()
		if id != "" {
			if task := s.confirmedResourceLocked(change, id); task != nil {
				return id, task, nil
			}
		}
		return "", nil, fmt.Errorf("task changed; refresh with sshore list")
	}
	id, task := s.findResourceLocked(change)
	if task == nil {
		return "", nil, fmt.Errorf("%s/%s no longer exists; refresh with sshore list", change.Kind, change.Ref)
	}
	return id, task, nil
}

func (s *daemonTasks) confirmedResourceLocked(change resourceMutation, id string) *daemonResourceTask {
	task, _ := s.lookupLocked(id).(*daemonResourceTask)
	if task != nil && task.matches(change) {
		return task
	}
	return nil
}

func (t *daemonResourceTask) matches(change resourceMutation) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.record
	return r.Kind == change.Kind && (r.Name == change.Ref || r.UID == change.Ref) &&
		(change.Identity == "" || resourceIdentity(r) == change.Identity)
}

func (s *daemonTasks) findResourceLocked(change resourceMutation) (string, *daemonResourceTask) {
	if validTaskUID(change.Ref) {
		if task, ok := s.lookupLocked(change.Ref).(*daemonResourceTask); ok && task.matches(change) {
			return change.Ref, task
		}
	}
	for id, registered := range s.liveLocked() {
		if task, ok := registered.(*daemonResourceTask); ok && task.matches(change) {
			return id, task
		}
	}
	return "", nil
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
	enc := json.NewEncoder(w)
	stops, err := s.confirmedResourceStops(changes)
	if err != nil {
		_ = enc.Encode(resourceReply{Error: err.Error()})
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
	_ = enc.Encode(resourceReply{Failures: failures})
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

func (s *resourceService) mountControl(action string, unmount chan<- chan error) resourceReply {
	if action == "status" {
		return resourceReply{Status: s.statusSnapshot()}
	}
	if action != "kill" {
		return resourceReply{Error: "invalid resource operation"}
	}
	request := make(chan error, 1)
	select {
	case unmount <- request:
	case <-s.done:
		return resourceReply{}
	}
	var err error
	select {
	case err = <-request:
	case <-s.done:
		// Shutdown can close done immediately after sending the stop result.
		select {
		case err = <-request:
		default:
		}
	}
	if err != nil {
		return resourceReply{Error: err.Error()}
	}
	return resourceReply{}
}

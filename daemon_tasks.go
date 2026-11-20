package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func nativeShells() ([]localShellView, error) {
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var views map[string]localShellView
	if err := daemonJSON(context.Background(), record, "/tasks/native", nil, &views); err != nil {
		return nil, err
	}
	rows := make([]localShellView, 0, len(views))
	for identity, view := range views {
		view.identity = identity
		view.ShortID = taskShortCode(identity)
		rows = append(rows, view)
	}
	sortLocalShells(rows)
	return rows, nil
}

func (s *daemonTasks) nativeViews() map[string]localShellView {
	tasks := liveTasks[*nativeShellTask](s)
	views := make(map[string]localShellView, len(tasks))
	for _, task := range tasks {
		view := task.shell.view()
		views[view.identity] = view
	}
	return views
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return errDaemonDraining
	}
	now := time.Now()
	_, isRequest := task.(requestRecord)
	if isRequest && task.expired(now) {
		return fmt.Errorf("request expired; request it again")
	}
	if s.lookupLocked(id, now) != nil {
		if isRequest {
			return fmt.Errorf("request already exists")
		}
		return fmt.Errorf("task already exists")
	}
	if err := s.checkAdmissionLocked("", task.taskName(), now, isRequest); err != nil {
		return err
	}
	if s.records == nil {
		s.records = make(map[string]daemonTask)
	}
	s.records[id] = task
	return nil
}

func (s *daemonTasks) checkNameLocked(except, name string, now time.Time) error {
	return s.checkAdmissionLocked(except, name, now, false)
}

// A new review counts pending requests in the same pass and reports capacity
// before a name conflict. Unnamed reviews do not reserve an alias.
func (s *daemonTasks) checkAdmissionLocked(except, name string, now time.Time, addingReview bool) error {
	pending := 0
	conflict := false
	for id, task := range s.liveLocked(now) {
		if addingReview {
			if _, ok := task.(requestRecord); ok {
				pending++
				if pending == maxPendingRequests {
					return fmt.Errorf("too many pending requests; use sshore list and cancel")
				}
			}
			if name == "" || conflict {
				continue
			}
		}
		if id != except && task.taskName() == name {
			conflict = true
			if !addingReview {
				break
			}
		}
	}
	if conflict {
		return fmt.Errorf("%s: %w", name, errObjectNameInUse)
	}
	return nil
}

// The caller holds the task table lock. Every scan uses the same expiry time
// and removes expired entries as it passes them.
func (s *daemonTasks) liveLocked(now time.Time) iter.Seq2[string, daemonTask] {
	return func(yield func(string, daemonTask) bool) {
		for id, task := range s.records {
			if task.expired(now) {
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
func (s *daemonTasks) lookupLocked(id string, now time.Time) daemonTask {
	task := s.records[id]
	if task != nil && task.expired(now) {
		delete(s.records, id)
		return nil
	}
	return task
}

func (s *daemonTasks) lookup(id string) daemonTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookupLocked(id, time.Now())
}

func liveTasks[T daemonTask](s *daemonTasks) []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []T
	for _, task := range s.liveLocked(time.Now()) {
		if t, ok := task.(T); ok {
			result = append(result, t)
		}
	}
	return result
}

func (s *daemonTasks) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
	return len(s.records) != 0
}

func (s *daemonTasks) pruneLocked(now time.Time) {
	for range s.liveLocked(now) {
	}
}

func (s *daemonTasks) availableName(uid, requested string, reuse bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	except := ""
	if reuse {
		except = uid
	}
	if requested != "" {
		if s.checkNameLocked(except, requested, now) != nil {
			return ""
		}
		return requested
	}
	// An automatic name can only collide with prefixes of this UID.
	var taken [33]bool
	for id, task := range s.liveLocked(now) {
		if id == except {
			continue
		}
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
	now := time.Now()
	if identity != "" {
		id := canonicalTaskUID(identity)
		if id == "" {
			return nil, localShellRecord{}, nil
		}
		task, ok := s.lookupLocked(id, now).(*nativeShellTask)
		if !ok {
			return nil, localShellRecord{}, nil
		}
		record := task.snapshot()
		if record.identity() != identity || (ref != "" && ref != record.Name && ref != record.UID) {
			return nil, localShellRecord{}, fmt.Errorf("local task changed; refresh with sshore list")
		}
		return task, record, nil
	}
	prefix := shortTaskPrefix(ref)
	if validTaskUID(prefix) {
		if task, ok := s.lookupLocked(prefix, now).(*nativeShellTask); ok {
			return task, task.snapshot(), nil
		}
		prefix = "" // A full UID that did not match can still be an alias.
	}
	var nameTask, prefixTask *nativeShellTask
	for id, candidate := range s.liveLocked(now) {
		task, ok := candidate.(*nativeShellTask)
		if !ok {
			continue
		}
		if prefix != "" && strings.HasPrefix(id, prefix) {
			if prefixTask != nil {
				return nil, localShellRecord{}, fmt.Errorf("ambiguous task ID")
			}
			prefixTask = task
			continue
		}
		if prefixTask == nil && task.taskName() == ref {
			nameTask = task
		}
	}
	if prefixTask != nil {
		return prefixTask, prefixTask.snapshot(), nil
	}
	if nameTask != nil {
		return nameTask, nameTask.snapshot(), nil
	}
	return nil, localShellRecord{}, nil
}

func (t *nativeShellTask) snapshot() localShellRecord {
	t.shell.mu.Lock()
	defer t.shell.mu.Unlock()
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
	Request   *requestRecord `json:",omitempty"`
	Unchanged bool           `json:",omitempty"`
	Status    resourceStatus
	Records   []resourceRecord `json:",omitempty"`
	Error     string           `json:",omitempty"`
	Failures  []string         `json:",omitempty"`
}

// A mount owns a child process; a forward owns an in-process executor. Both
// expose the same bounded control operations through the account task table.
type daemonResourceTask struct {
	mu            sync.Mutex
	record        resourceRecord // UID and Host are immutable after registration.
	done          <-chan struct{}
	control       func(string) resourceReply
	statusPending atomic.Bool
	finalizing    bool
	stopMu        sync.Mutex
	stopAccepted  bool // Protected by stopMu.
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

const resourceStatusTimeout = 2 * time.Second

type resourceStatusRow struct {
	UID    string
	Status resourceStatus
}

func (s *daemonTasks) serveResources(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	withStatus := query.Get("status") == "1"
	records, workers := s.resourceRecords(query.Get("host"), withStatus)
	enc := json.NewEncoder(w)
	if withStatus {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	if err := enc.Encode(resourceReply{Records: records}); err != nil || !withStatus || len(workers) == 0 {
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
			if row.UID != "" {
				if err := enc.Encode(row); err != nil {
					return
				}
				if err := controller.Flush(); err != nil {
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (task *daemonResourceTask) readStatus() resourceStatusRow {
	if task.expired(time.Now()) || !task.statusPending.CompareAndSwap(false, true) {
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
	if t.stopAccepted || t.expired(time.Time{}) {
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

func (t *daemonResourceTask) stop(timeout time.Duration) error {
	if err := t.requestStop(); err != nil {
		return err
	}
	if timeout == 0 {
		<-t.done
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.done:
		return nil
	case <-timer.C:
		return errors.New("resource cleanup is still pending")
	}
}

func (t *daemonResourceTask) finalName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finalizing = true
	return t.record.Name
}

// Startup supplies runtime details. The task table owns identity and naming.
func (t *daemonResourceTask) publishReady(detail, root, mountpoint string) resourceRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.record.Detail, t.record.Remote, t.record.Mountpoint = detail, root, mountpoint
	return t.record
}

func (s *daemonTasks) resourceRecords(host string, withStatus bool) ([]resourceRecord, []*daemonResourceTask) {
	var records []resourceRecord
	var workers []*daemonResourceTask
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.liveLocked(time.Now()) {
		switch t := task.(type) {
		case *daemonResourceTask:
			if host != "" && t.record.Host != host {
				continue
			}
			// Capture each alias while the table still excludes renames and
			// admission. Status probes operate on the captured workers later.
			t.mu.Lock()
			records = append(records, t.record)
			t.mu.Unlock()
			if withStatus {
				workers = append(workers, t)
			}
		case requestRecord:
			if host != "" && t.Connection.Host != host {
				continue
			}
			records = append(records, resourceRecord{
				UID: t.ID, Kind: "review", Name: t.DisplayName(), Host: t.Connection.Host,
				Detail: t.Connection.Session + ": " + requestSummary(t.Operations),
			})
		}
	}
	return records, workers
}

func (s *daemonTasks) serveResourceMutation(w http.ResponseWriter, r *http.Request) {
	var mutation resourceMutation
	if !readDaemonBody(w, r, &mutation) {
		return
	}
	reply, task, err := s.mutateResource(r.PathValue("action"), mutation)
	if err == nil && task != nil {
		err = task.stop(resourceStopTimeout)
	}
	if err != nil {
		reply.Error = err.Error()
	}
	_ = json.NewEncoder(w).Encode(reply)
}

// A stop returns the exact registered worker to stop outside the task table
// lock. Renames and request removals complete under the same lock as lookup.
func (s *daemonTasks) mutateResource(action string, change resourceMutation) (resourceReply, *daemonResourceTask, error) {
	if action != "kill" && action != "rename" {
		return resourceReply{}, nil, fmt.Errorf("invalid resource operation")
	}
	if err := validSession(change.Ref); err != nil {
		return resourceReply{}, nil, err
	}
	if action == "rename" {
		if err := validAlias(change.NewName); err != nil {
			return resourceReply{}, nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	id, target, err := s.resolveResourceLocked(change, now)
	if err != nil {
		return resourceReply{}, nil, err
	}
	if record, ok := target.(requestRecord); ok {
		if action == "kill" {
			delete(s.records, id)
			return resourceReply{Request: &record}, nil, nil
		}
		if record.DisplayName() == change.NewName {
			return resourceReply{Unchanged: true}, nil, nil
		}
		if err := s.checkNameLocked(id, change.NewName, now); err != nil {
			return resourceReply{}, nil, err
		}
		record.Name = change.NewName
		s.records[id] = record
		return resourceReply{}, nil, nil
	}

	task := target.(*daemonResourceTask)
	if action == "kill" {
		return resourceReply{}, task, nil
	}
	if err := s.checkNameLocked(id, change.NewName, now); err != nil {
		return resourceReply{}, nil, err
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finalizing {
		return resourceReply{}, nil, fmt.Errorf("task is exiting; refresh with sshore list")
	}
	task.record.Name = change.NewName
	return resourceReply{}, nil, nil
}

func (s *daemonTasks) resolveResourceLocked(change resourceMutation, now time.Time) (string, daemonTask, error) {
	switch change.Kind {
	case "review", "mount", "forward":
	default:
		return "", nil, fmt.Errorf("invalid resource type")
	}
	if change.Identity != "" {
		id := change.confirmedUID()
		if id != "" {
			if task := s.confirmedResourceLocked(change, id, now); task != nil {
				return id, task, nil
			}
		}
		if change.Kind == "review" {
			return "", nil, fmt.Errorf("request changed; refresh with sshore list")
		}
		return "", nil, fmt.Errorf("task changed; refresh with sshore list")
	}
	if change.Kind == "review" {
		record, err := s.findRequestLocked(change.Ref, now)
		return record.ID, record, err
	}
	id, task := s.findResourceLocked(change, now)
	if task == nil {
		return "", nil, fmt.Errorf("%s/%s no longer exists; refresh with sshore list", change.Kind, change.Ref)
	}
	return id, task, nil
}

func (s *daemonTasks) confirmedResourceLocked(change resourceMutation, id string, now time.Time) daemonTask {
	switch task := s.lookupLocked(id, now).(type) {
	case requestRecord:
		if change.Kind == "review" && task.ID == id && (change.Ref == task.DisplayName() || change.Ref == id) {
			return task
		}
	case *daemonResourceTask:
		if (change.Kind == "mount" || change.Kind == "forward") && task.matches(change) {
			return task
		}
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

func (s *daemonTasks) findResourceLocked(change resourceMutation, now time.Time) (string, *daemonResourceTask) {
	if validTaskUID(change.Ref) {
		if task, ok := s.lookupLocked(change.Ref, now).(*daemonResourceTask); ok && task.matches(change) {
			return change.Ref, task
		}
	}
	for id, registered := range s.liveLocked(now) {
		if task, ok := registered.(*daemonResourceTask); ok && task.matches(change) {
			return id, task
		}
	}
	return "", nil
}

// Resolve all confirmed identities before removing requests or stopping workers.
func (s *daemonTasks) confirmedResourceStops(changes []resourceMutation) ([]*daemonResourceTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	stops := make([]*daemonResourceTask, len(changes))
	// Membership detects duplicates. The value identifies requests to remove
	// once every selected task has been verified.
	selected := make(map[string]bool, len(changes))
	for i, change := range changes {
		id := change.confirmedUID()
		_, duplicate := selected[id]
		if id == "" || duplicate || validSession(change.Ref) != nil {
			return nil, fmt.Errorf("invalid confirmed resource selection")
		}
		switch change.Kind {
		case "review":
			if _, ok := s.confirmedResourceLocked(change, id, now).(requestRecord); !ok {
				return nil, fmt.Errorf("request changed; refresh with sshore list")
			}
			selected[id] = true
		case "mount", "forward":
			task, ok := s.confirmedResourceLocked(change, id, now).(*daemonResourceTask)
			if !ok {
				return nil, fmt.Errorf("%s/%s changed; review again", change.Kind, change.Ref)
			}
			stops[i] = task
			selected[id] = false
		default:
			return nil, fmt.Errorf("invalid confirmed resource type")
		}
	}
	for id, remove := range selected {
		if remove {
			delete(s.records, id)
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
	pending := 0
	for i, task := range stops {
		if task != nil {
			pending++
			go func() {
				if err := task.stop(resourceStopTimeout); err != nil {
					failures[i] = err.Error()
				}
				finished <- struct{}{}
			}()
		}
	}
	for range pending {
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

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/files"
)

const requestTTL = 10 * time.Minute
const maxRequestItems = 16
const maxPendingRequests = 32
const maxRequestRecord = 128 * 1024

type operation struct {
	Kind       string `json:"kind"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	Path       string `json:"path,omitempty"`
	Name       string `json:"name,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Port       int    `json:"port,omitempty"`
	Reverse    bool   `json:"reverse,omitempty"`
	TargetPort int    `json:"target_port,omitempty"`
	LocalHint  string `json:"local_hint,omitempty"`
	LocalPath  string `json:"-"`
}

type requestRecord struct {
	Connection             recentConnection
	PID                    int
	SoftwareVersion        string
	ID                     string
	Name                   string
	Created                time.Time
	Directory, DownloadDir string
	Quiet                  bool
	Operations             []operation
	Setup                  *setupReview
	decision               *setupDecision
}

func (r requestRecord) DisplayName() string {
	if r.Name != "" {
		return r.Name
	}
	return r.ID
}

// Port always denotes the listener, TargetPort the service, including pending requests.
func (op operation) ServerPort() int {
	if op.Reverse {
		return op.Port
	}
	return op.TargetPort
}

func (op operation) LocalPortPending() bool {
	return op.Kind == "forward" && (op.Port == 0 || op.TargetPort == 0)
}

func validateRequestOperations(ops []operation) error {
	if len(ops) == 0 || len(ops) > maxRequestItems {
		return fmt.Errorf("request must contain 1–%d operations", maxRequestItems)
	}
	var total int64
	for _, op := range ops {
		if len(op.LocalHint) > 4096 || strings.ContainsAny(op.LocalHint, "\x00\r\n") {
			return fmt.Errorf("invalid local endpoint hint")
		}
		if op.ReadOnly && op.Kind != "mount" {
			return fmt.Errorf("unexpected read-only flag")
		}
		switch op.Kind {
		case "download", "upload", "mount":
			// The remote server is Linux, even when the client is Windows.
			if !strings.HasPrefix(op.Path, "/") || len(op.Path) > 4096 || strings.ContainsAny(op.Path, "\x00\r\n") {
				return fmt.Errorf("invalid remote path")
			}
			if op.Port != 0 || op.TargetPort != 0 || op.Reverse {
				return fmt.Errorf("unexpected port in file request")
			}
			if op.Kind == "mount" && len(ops) != 1 {
				return fmt.Errorf("mount requests cannot be batched")
			}
			if op.Kind == "download" {
				if err := files.ValidateName(op.Name); err != nil {
					return err
				}
				if op.Size < 0 || op.Size > (1<<63-1)-total {
					return fmt.Errorf("invalid download size")
				}
				total += op.Size
			} else if op.Name != "" || op.Size != 0 {
				return fmt.Errorf("unexpected upload metadata")
			}
		case "forward":
			if op.LocalHint != "" {
				if _, err := parseForwardPort(op.LocalHint); err != nil {
					return err
				}
			}
			if len(ops) != 1 || op.Port < 0 || op.Port > 65535 || op.TargetPort < 0 || op.TargetPort > 65535 || op.ServerPort() == 0 || op.Path != "" || op.Name != "" || op.Size != 0 {
				return fmt.Errorf("invalid port request")
			}
		default:
			return fmt.Errorf("unsupported requested operation")
		}
	}
	return nil
}

// Pending requests belong to the account daemon. Selected local paths and ports
// remain on the caller's independent copy; consumption deletes the pending intent.

func validRequestID(id string) bool { return validTaskUID(id) }

func cloneRequest(r requestRecord) requestRecord {
	r.Connection = r.Connection.clone()
	r.Operations = slices.Clone(r.Operations)
	if r.Setup != nil {
		setup := *r.Setup
		setup.Tasks = slices.Clone(setup.Tasks)
		r.Setup = &setup
	}
	return r
}
func (s *daemonTasks) prune(now time.Time) {
	for id, r := range s.records {
		if r.expired(now) {
			s.removeRequest(id)
		}
	}
}
func (s *daemonTasks) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	return len(s.records) != 0
}
func (s *daemonTasks) Save(r requestRecord) error {
	if r.Setup != nil {
		return fmt.Errorf("setup requires a waiting client")
	}
	if !validRequestID(r.ID) {
		return fmt.Errorf("invalid request ID")
	}
	if !sameRelease(version, r.SoftwareVersion) {
		return fmt.Errorf("incompatible request release")
	}
	now := time.Now()
	if now.Sub(r.Created) > requestTTL || r.Created.After(now.Add(time.Minute)) {
		return fmt.Errorf("request expired; request it again")
	}
	if err := validateRequestOperations(r.Operations); err != nil {
		return err
	}
	if err := validateRequestRecord(r); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > maxRequestRecord {
		return fmt.Errorf("request record too large")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertRequest(r, now)
}

func (s *daemonTasks) insertRequest(r requestRecord, now time.Time) error {
	if s.draining {
		return fmt.Errorf("daemon is shutting down")
	}
	s.prune(now)
	if _, exists := s.records[r.ID]; exists {
		return fmt.Errorf("request already exists")
	}
	pending := 0
	for _, task := range s.records {
		if _, ok := task.(requestRecord); ok {
			pending++
		}
	}
	if pending >= maxPendingRequests {
		return fmt.Errorf("too many pending requests; use sshore list and cancel")
	}
	if r.Name != "" {
		for _, old := range s.records {
			if old.taskName() == r.Name {
				return fmt.Errorf("%s: %w", r.Name, errObjectNameInUse)
			}
		}
	}
	if s.records == nil {
		s.records = make(map[string]daemonTask)
	}
	r = cloneRequest(r)
	for i := range r.Operations {
		r.Operations[i].LocalPath = ""
	}
	s.records[r.ID] = r
	return nil
}
func (s *daemonTasks) Read(id string) (requestRecord, error) {
	if !validRequestID(id) {
		return requestRecord{}, fmt.Errorf("invalid request ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	task, found := s.records[id]
	r, isRequest := task.(requestRecord)
	if !found || !isRequest {
		return requestRecord{}, fmt.Errorf("request no longer exists or expired: %w", os.ErrNotExist)
	}
	return cloneRequest(r), nil
}
func (s *daemonTasks) Pending() []requestRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	records := make([]requestRecord, 0, len(s.records))
	for _, r := range s.records {
		if r, ok := r.(requestRecord); ok {
			records = append(records, cloneRequest(r))
		}
	}
	slices.SortFunc(records, func(a, b requestRecord) int {
		if (a.Setup != nil) != (b.Setup != nil) {
			if a.Setup != nil {
				return -1
			}
			return 1
		}
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
	return records
}
func (s *daemonTasks) Find(ref string) (requestRecord, error) {
	if validRequestID(ref) {
		return s.Read(ref)
	}
	if err := validSession(ref); err != nil {
		return requestRecord{}, err
	}
	records := s.Pending()
	var found *requestRecord
	for _, r := range records {
		if isShortTaskID(ref) && strings.HasPrefix(r.ID, strings.ToLower(ref)) {
			if found != nil {
				return requestRecord{}, fmt.Errorf("ambiguous request ID %s", ref)
			}
			copy := r
			found = &copy
		}
	}
	if found != nil {
		return *found, nil
	}
	for _, r := range records {
		if r.Name == ref {
			return r, nil
		}
	}
	return requestRecord{}, fmt.Errorf("request no longer exists or expired: %w", os.ErrNotExist)
}
func (s *daemonTasks) Consume(id string) error {
	if !validRequestID(id) {
		return fmt.Errorf("invalid request ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	if _, found := s.records[id].(requestRecord); !found {
		return fmt.Errorf("request no longer exists or expired: %w", os.ErrNotExist)
	}
	s.removeRequest(id)
	return nil
}
func (s *daemonTasks) Rename(id, name string) error {
	if !validRequestID(id) {
		return fmt.Errorf("invalid request ID")
	}
	if err := validSession(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	task, found := s.records[id]
	r, isRequest := task.(requestRecord)
	if !found || !isRequest {
		return fmt.Errorf("request no longer exists or expired: %w", os.ErrNotExist)
	}
	for key, old := range s.records {
		if key != id && old.taskName() == name {
			return fmt.Errorf("%s: %w", name, errObjectNameInUse)
		}
	}
	r.Name = name
	s.records[id] = r
	return nil
}

type requestRPCReply struct {
	Request  *requestRecord  `json:",omitempty"`
	Requests []requestRecord `json:",omitempty"`
	Error    string          `json:",omitempty"`
	Queued   string          `json:",omitempty"`
	NotFound bool            `json:",omitempty"`
}

func (s *daemonTasks) Serve(c net.Conn, m message) {
	reply := requestRPCReply{}
	var err error
	switch m.Action {
	case "setup_wait":
		err = s.waitSetup(c)
	case "setup_approve":
		err = s.approveSetup(m.ApprovalID)
	case "request_save":
		var r requestRecord
		err = json.NewDecoder(io.LimitReader(c, maxRequestRecord+1)).Decode(&r)
		if err == nil {
			err = s.Save(r)
		}
	case "request_get":
		var r requestRecord
		r, err = s.Find(m.Name)
		if err == nil {
			reply.Request = &r
		}
	case "request_list":
		reply.Requests = s.Pending()
	case "request_consume":
		err = s.Consume(m.ApprovalID)
	case "request_rename":
		err = s.Rename(m.ApprovalID, m.Name)
	default:
		err = fmt.Errorf("invalid request store action")
	}
	if err != nil {
		reply.Error = err.Error()
		reply.NotFound = errors.Is(err, os.ErrNotExist)
	}
	_ = json.NewEncoder(c).Encode(reply)
}
func requestRPC(action, ref, name string, request *requestRecord) (requestRPCReply, error) {
	return requestRPCContext(context.Background(), requestTimeout, action, ref, name, request, nil)
}
func requestRPCContext(parent context.Context, timeout time.Duration, action, ref, name string, request *requestRecord, queued func(string)) (requestRPCReply, error) {
	var reply requestRPCReply
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	var record localShellRecord
	var err error
	if action == "request_save" || action == "setup_wait" {
		record, err = ensureLocalDaemonContext(ctx)
	} else {
		record, err = existingDaemonRecord()
	}
	if err != nil {
		return reply, err
	}
	c, err := dialLocalSessionContext(ctx, record, time.Now().Add(timeout))
	if err != nil {
		return reply, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if err := writeMessage(c, message{Type: "request_store", Action: action, Name: name, ApprovalID: ref}); err != nil {
		return reply, err
	}
	if request != nil {
		if err := json.NewEncoder(c).Encode(request); err != nil {
			return reply, err
		}
	}
	decoder := json.NewDecoder(io.LimitReader(c, maxPendingRequests*maxRequestRecord+4096))
	if err := decoder.Decode(&reply); err != nil {
		return reply, err
	}
	if queued != nil && reply.Error == "" {
		if !validRequestID(reply.Queued) {
			return reply, fmt.Errorf("invalid setup queue reply")
		}
		queued(reply.Queued)
		reply = requestRPCReply{}
		if err := decoder.Decode(&reply); err != nil {
			return reply, err
		}
	}
	if reply.NotFound {
		return reply, fmt.Errorf("%s: %w", reply.Error, os.ErrNotExist)
	}
	if reply.Error != "" {
		return reply, fmt.Errorf("%s", reply.Error)
	}
	return reply, nil
}
func saveRequest(r requestRecord) error {
	_, err := requestRPC("request_save", "", "", &r)
	return err
}
func pendingRequests() ([]requestRecord, error) {
	result, err := requestRPC("request_list", "", "", nil)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return result.Requests, err
}
func findRequest(ref string) (requestRecord, error) {
	result, err := requestRPC("request_get", "", ref, nil)
	if err != nil {
		return requestRecord{}, err
	}
	if result.Request == nil {
		return requestRecord{}, fmt.Errorf("invalid request store response")
	}
	return *result.Request, nil
}
func readRequest(id string) (requestRecord, error) {
	if !validRequestID(id) {
		return requestRecord{}, fmt.Errorf("invalid request ID")
	}
	return findRequest(id)
}
func consumeRequest(id string) error {
	_, err := requestRPC("request_consume", id, "", nil)
	return err
}
func renameRequest(id, name string) error {
	_, err := requestRPC("request_rename", id, name, nil)
	return err
}

// The waiting SSH connection owns execution. Review only releases the exact
// proposal held in the local daemon. Decisions never survive daemon restart.
type setupReview struct {
	Installed string
	Target    string
	Account   string
	Tasks     []setupProcess
}
type setupDecision struct {
	done     chan struct{}
	approved bool
	waiters  int
}

func (r requestRecord) summary() string {
	if r.Setup != nil {
		return "setup " + r.Setup.Installed + " → " + r.SoftwareVersion
	}
	return requestSummary(r.Operations)
}

// removeRequest runs under s.mu. Ordinary cancellation, expiry and shutdown
// all wake the waiting connection with a negative decision.
func (s *daemonTasks) removeRequest(id string) {
	if r, ok := s.records[id].(requestRecord); ok && r.decision != nil {
		close(r.decision.done)
	}
	delete(s.records, id)
}
func (s *daemonTasks) approveSetup(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	r, ok := s.records[id].(requestRecord)
	if !ok || r.decision == nil {
		return fmt.Errorf("setup request expired or cancelled")
	}
	r.decision.approved = true
	s.removeRequest(id)
	return nil
}

func (s *daemonTasks) waitSetup(c net.Conn) error {
	var r requestRecord
	if err := json.NewDecoder(io.LimitReader(c, maxRequestRecord)).Decode(&r); err != nil {
		return err
	}
	if !validRequestID(r.ID) || r.SoftwareVersion != version || r.Setup == nil || len(r.Operations) != 0 || validSSHDestination(r.Connection.Host) != nil {
		return fmt.Errorf("invalid setup request")
	}
	s.mu.Lock()
	now := time.Now()
	s.prune(now)
	for _, task := range s.records {
		old, ok := task.(requestRecord)
		if ok && old.decision != nil && reflect.DeepEqual(old.Connection, r.Connection) && reflect.DeepEqual(old.Setup, r.Setup) {
			r = old
			break
		}
	}
	if r.decision == nil {
		r.Created = now
		r.Name = ""
		r.decision = &setupDecision{done: make(chan struct{})}
		if err := s.insertRequest(r, now); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	d := r.decision
	d.waiters++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		d.waiters--
		if old, ok := s.records[r.ID].(requestRecord); ok && old.decision == d && d.waiters == 0 {
			s.removeRequest(r.ID)
		}
	}()
	if err := json.NewEncoder(c).Encode(requestRPCReply{Queued: r.ID}); err != nil {
		return err
	}
	_ = c.SetDeadline(r.Created.Add(requestTTL))
	disconnected := make(chan struct{})
	go func() {
		var b [1]byte
		_, _ = c.Read(b[:])
		close(disconnected)
	}()
	select {
	case <-d.done:
		if d.approved {
			return nil
		}
	case <-disconnected:
	}
	return fmt.Errorf("setup review declined, cancelled or expired")
}

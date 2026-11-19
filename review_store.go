package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	r.Connection = *r.Connection.clone()
	r.Operations = slices.Clone(r.Operations)
	return r
}
func (r requestRecord) validate(now time.Time) error {
	if !validRequestID(r.ID) {
		return fmt.Errorf("invalid request ID")
	}
	if !sameRelease(version, r.SoftwareVersion) {
		return fmt.Errorf("incompatible request release")
	}
	if r.expired(now) || r.Created.After(now.Add(time.Minute)) {
		return fmt.Errorf("request expired; request it again")
	}
	if err := validateRequestOperations(r.Operations); err != nil {
		return err
	}
	if err := validateRecentConnection(r.Connection); err != nil {
		return fmt.Errorf("saved connection is incompatible; request it again: %w", err)
	}
	if r.PID <= 0 || !validTaskUID(r.Connection.Instance) {
		return fmt.Errorf("invalid review identity")
	}
	if r.Name != "" && validSession(r.Name) != nil {
		return fmt.Errorf("invalid review name")
	}
	if !filepath.IsAbs(r.Directory) {
		return fmt.Errorf("invalid local approval directory")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > maxRequestRecord {
		return fmt.Errorf("request record too large")
	}
	return nil
}

func (s *daemonTasks) Save(r requestRecord) error {
	now := time.Now()
	if err := r.validate(now); err != nil {
		return err
	}
	return s.register(r.ID, cloneRequest(r))
}

func (s *daemonTasks) Pending() []requestRecord {
	records := liveTasks[requestRecord](s)
	for i, r := range records {
		records[i] = cloneRequest(r)
	}
	slices.SortFunc(records, func(a, b requestRecord) int {
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
	return records
}
func (s *daemonTasks) Find(ref string) (requestRecord, error) {
	s.mu.Lock()
	r, err := s.findRequestLocked(ref, time.Now())
	s.mu.Unlock()
	if err != nil {
		return requestRecord{}, err
	}
	// The stored connection and operations are immutable. Copy after lookup so
	// other tasks do not wait for the route and operation slices to be cloned.
	return cloneRequest(r), nil
}

func (s *daemonTasks) findRequestLocked(ref string, now time.Time) (requestRecord, error) {
	if validRequestID(ref) {
		if r, found := s.lookupLocked(ref, now).(requestRecord); found {
			return r, nil
		}
		return requestRecord{}, fmt.Errorf("request no longer exists or expired: %w", os.ErrNotExist)
	}
	if err := validSession(ref); err != nil {
		return requestRecord{}, err
	}
	var prefixMatch, nameMatch requestRecord
	prefix := shortTaskPrefix(ref)
	for id, task := range s.liveLocked(now) {
		r, ok := task.(requestRecord)
		if !ok {
			continue
		}
		if prefix != "" && strings.HasPrefix(id, prefix) {
			if prefixMatch.ID != "" {
				return requestRecord{}, fmt.Errorf("ambiguous request ID %s", ref)
			}
			prefixMatch = r
		}
		if r.Name == ref {
			nameMatch = r
		}
	}
	if prefixMatch.ID != "" {
		return prefixMatch, nil
	}
	if nameMatch.ID != "" {
		return nameMatch, nil
	}
	return requestRecord{}, fmt.Errorf("request no longer exists or expired: %w", os.ErrNotExist)
}

func (s *daemonTasks) Consume(id string) error {
	if !validRequestID(id) {
		return fmt.Errorf("invalid request ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.findRequestLocked(id, time.Now())
	if err == nil {
		delete(s.records, record.ID)
	}
	return err
}

type requestRPCReply struct {
	Request  *requestRecord  `json:",omitempty"`
	Requests []requestRecord `json:",omitempty"`
	Error    string          `json:",omitempty"`
	NotFound bool            `json:",omitempty"`
}

func (s *daemonTasks) serveRequest(w http.ResponseWriter, r *http.Request) {
	reply := requestRPCReply{}
	var err error
	ref := r.URL.Query().Get("ref")
	switch r.PathValue("action") {
	case "save":
		var record requestRecord
		err = decodeDaemonBody(w, r, maxRequestRecord+1, &record)
		if err == nil {
			err = s.Save(record)
		}
	case "get":
		var record requestRecord
		record, err = s.Find(ref)
		if err == nil {
			reply.Request = &record
		}
	case "list":
		reply.Requests = s.Pending()
	case "consume":
		err = s.Consume(ref)
	default:
		err = fmt.Errorf("invalid request store action")
	}
	if err != nil {
		reply.Error = err.Error()
		reply.NotFound = errors.Is(err, os.ErrNotExist)
	}
	_ = json.NewEncoder(w).Encode(reply)
}
func requestRPC(action, ref string, request *requestRecord) (requestRPCReply, error) {
	var reply requestRPCReply
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var record localShellRecord
	var err error
	if action == "save" {
		record, err = ensureLocalDaemonContext(ctx)
	} else {
		record, err = existingDaemonRecord()
	}
	if err != nil {
		return reply, err
	}
	path := "/requests/" + action
	if ref != "" {
		path += "?ref=" + url.QueryEscape(ref)
	}
	if err := daemonJSONLimit(ctx, record, path, request, &reply, maxPendingRequests*maxRequestRecord+4096); err != nil {
		return reply, err
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
	_, err := requestRPC("save", "", &r)
	return err
}
func pendingRequests() ([]requestRecord, error) {
	result, err := requestRPC("list", "", nil)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return result.Requests, err
}
func findRequest(ref string) (requestRecord, error) {
	result, err := requestRPC("get", ref, nil)
	if err != nil {
		return requestRecord{}, err
	}
	if result.Request == nil {
		return requestRecord{}, fmt.Errorf("invalid request store response")
	}
	return *result.Request, nil
}
func consumeRequest(id string) error {
	_, err := requestRPC("consume", id, nil)
	return err
}

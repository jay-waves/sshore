package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/transport"
)

const approvalTTL = 10 * time.Minute
const maxApprovalItems = 16
const maxPendingApprovals = 32
const maxApprovalRecord = 128 * 1024

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

type approvalRecord struct {
	PID                                           int
	SoftwareVersion                               string
	ID                                            string
	Name                                          string
	Created                                       time.Time
	Host, Session, Server, Directory, DownloadDir string
	SSH                                           []string
	Quiet, NoEscape                               bool
	Operations                                    []operation
}

func (r approvalRecord) DisplayName() string {
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

func validateApprovalOperations(ops []operation) error {
	if len(ops) == 0 || len(ops) > maxApprovalItems {
		return fmt.Errorf("request must contain 1–%d operations", maxApprovalItems)
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

// approvalStorage owns bounded approval files and their atomic, single-use claims.
type approvalStorage struct {
	directory string
}

func validApprovalID(id string) bool {
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 16 && id == strings.ToLower(id)
}
func (s approvalStorage) CheckCapacity() error {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return err
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			info, err := e.Info()
			if err != nil {
				return err
			}
			if time.Since(info.ModTime()) > approvalTTL {
				_ = os.Remove(filepath.Join(s.directory, e.Name()))
				continue
			}
			count++
		}
	}
	if count >= maxPendingApprovals {
		return fmt.Errorf("too many pending approvals; use 'sshore list' and 'sshore cancel review/NAME'")
	}
	return nil
}
func (s approvalStorage) Save(r approvalRecord) error {
	if !validApprovalID(r.ID) {
		return fmt.Errorf("invalid approval ID")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > maxApprovalRecord {
		return fmt.Errorf("approval record too large")
	}
	path := filepath.Join(s.directory, r.ID+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if err = privateStatePath(path, false); err != nil {
		return err
	}
	if err = transport.WriteFull(f, data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
func (s approvalStorage) Read(id string) (approvalRecord, error) {
	var r approvalRecord
	if !validApprovalID(id) {
		return r, fmt.Errorf("invalid approval ID")
	}
	path := filepath.Join(s.directory, id+".json")
	if err := privateStatePath(path, false); err != nil {
		return r, err
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxApprovalRecord+1))
	if err != nil {
		return r, err
	}
	if len(data) > maxApprovalRecord {
		return r, fmt.Errorf("approval record too large")
	}
	if err = json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if version == "" || version != r.SoftwareVersion || r.ID != id || time.Since(r.Created) > approvalTTL || r.Created.After(time.Now().Add(time.Minute)) {
		return r, fmt.Errorf("approval expired or incompatible; request it again")
	}
	if err := validateApprovalOperations(r.Operations); err != nil {
		return r, err
	}
	return r, validateApprovalRecord(r)
}
func (s approvalStorage) Pending() ([]approvalRecord, error) {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return nil, err
	}
	var records []approvalRecord
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		r, err := s.Read(strings.TrimSuffix(entry.Name(), ".json"))
		if err == nil {
			records = append(records, r)
		}
	}
	return records, nil
}

// Exclusive creation serializes claims across processes before renaming. Concurrent
// Windows DeleteFile calls can both succeed while a file is delete-pending.
// Only one concurrent approver can succeed; failed executions cannot be replayed.
func (s approvalStorage) Consume(id string) error {
	if !validApprovalID(id) {
		return fmt.Errorf("invalid approval ID")
	}
	lockPath := filepath.Join(s.directory, id+".claim")
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		_ = lock.Close()
		_ = os.Remove(lockPath)
	}()
	claimed := filepath.Join(s.directory, id+".consumed")
	if err := os.Rename(filepath.Join(s.directory, id+".json"), claimed); err != nil {
		return err
	}
	_ = os.Remove(claimed)
	return nil
}

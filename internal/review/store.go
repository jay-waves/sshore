package review

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/transport"
)

// Store owns bounded approval files and their atomic, single-use claims.
// The program supplies platform privacy checks and connection-policy validation.
type Store struct {
	Directory, Version string
	Private            func(string, bool) error
	Validate           func(Record) error
}

func ValidID(id string) bool {
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 16 && id == strings.ToLower(id)
}
func (s Store) CheckCapacity() error {
	entries, err := os.ReadDir(s.Directory)
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
			if time.Since(info.ModTime()) > TTL {
				_ = os.Remove(filepath.Join(s.Directory, e.Name()))
				continue
			}
			count++
		}
	}
	if count >= MaxPending {
		return fmt.Errorf("too many pending approvals; use 'sshore list' and 'sshore cancel review/NAME'")
	}
	return nil
}
func (s Store) Save(r Record) error {
	if !ValidID(r.ID) {
		return fmt.Errorf("invalid approval ID")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > MaxRecord {
		return fmt.Errorf("approval record too large")
	}
	path := filepath.Join(s.Directory, r.ID+".json")
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
	if err = s.Private(path, false); err != nil {
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
func (s Store) Read(id string) (Record, error) {
	var r Record
	if !ValidID(id) {
		return r, fmt.Errorf("invalid approval ID")
	}
	path := filepath.Join(s.Directory, id+".json")
	if err := s.Private(path, false); err != nil {
		return r, err
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxRecord+1))
	if err != nil {
		return r, err
	}
	if len(data) > MaxRecord {
		return r, fmt.Errorf("approval record too large")
	}
	if err = json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if s.Version == "" || s.Version != r.SoftwareVersion || r.ID != id || time.Since(r.Created) > TTL || r.Created.After(time.Now().Add(time.Minute)) {
		return r, fmt.Errorf("approval expired or incompatible; request it again")
	}
	if err := ValidateOperations(r.Operations); err != nil {
		return r, err
	}
	return r, s.Validate(r)
}
func (s Store) Pending() ([]Record, error) {
	entries, err := os.ReadDir(s.Directory)
	if err != nil {
		return nil, err
	}
	var records []Record
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
func (s Store) Consume(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid approval ID")
	}
	lockPath := filepath.Join(s.Directory, id+".claim")
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		_ = lock.Close()
		_ = os.Remove(lockPath)
	}()
	claimed := filepath.Join(s.Directory, id+".consumed")
	if err := os.Rename(filepath.Join(s.Directory, id+".json"), claimed); err != nil {
		return err
	}
	_ = os.Remove(claimed)
	return nil
}

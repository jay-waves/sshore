package main

// Status separates local worker liveness from transport state and target health.
// Target services are never probed by listing. Exported views deliberately omit
// control capabilities, addresses, SSH options, credentials and terminal output.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type resourceStatus struct {
	Version       string    `json:"version"`
	Transport     string    `json:"transport"`
	Target        string    `json:"target"`
	Started       time.Time `json:"started,omitempty"`
	LastOperation time.Time `json:"last_operation,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}
type resourceStatusState struct {
	mu    sync.Mutex
	value resourceStatus
}

func (s *resourceService) statusSnapshot() resourceStatus {
	s.status.mu.Lock()
	defer s.status.mu.Unlock()
	return s.status.value
}
func (s *resourceService) setTransport(state string, err error) {
	if s == nil {
		return
	}
	s.status.mu.Lock()
	defer s.status.mu.Unlock()
	s.status.value.Version, s.status.value.Transport, s.status.value.Target = version, state, "unchecked"
	if err != nil {
		s.status.value.LastError = cleanText(err.Error())
	}
	if state == "connected" && s.status.value.Started.IsZero() {
		s.status.value.Started = time.Now().UTC()
	}
}
func (s *resourceService) operationCompleted() {
	if s == nil {
		return
	}
	s.status.mu.Lock()
	defer s.status.mu.Unlock()
	s.status.value.LastOperation = time.Now().UTC()
}
func (s *resourceService) saveResult(code int, err error) {
	state := "stopped"
	if code != 0 || err != nil {
		state = "failed"
	}
	s.setTransport(state, err)
	// A bounded receipt per resource name, overwritten on the next lifecycle.
	r := resourceResult{Record: s.record, Status: s.statusSnapshot(), Finished: time.Now().UTC()}
	r.Record.Token, r.Record.Address = "", ""
	data, e := json.Marshal(r)
	if e != nil {
		return
	}
	dir := filepath.Dir(s.recordPath)
	f, e := os.CreateTemp(dir, ".result-*")
	if e != nil {
		return
	}
	defer os.Remove(f.Name())
	if privateState(f.Name(), false) != nil {
		f.Close()
		return
	}
	if _, e = f.Write(data); e == nil {
		e = f.Sync()
	}
	f.Close()
	if e == nil {
		_ = os.Rename(f.Name(), s.recordPath+".result")
		pruneResourceResults(dir)
	}
}

type resourceResult struct {
	Record   resourceRecord
	Status   resourceStatus
	Finished time.Time
}

// Retain at most 100 receipts per resource kind for seven days. Active records
// are separate files and are never removed by history cleanup.
func pruneResourceResults(dir string) {
	entries, e := os.ReadDir(dir)
	if e != nil {
		return
	}
	type resultFile struct {
		name string
		at   time.Time
	}
	var files []resultFile
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json.result") {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			continue
		}
		files = append(files, resultFile{entry.Name(), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.After(files[j].at) })
	for i, f := range files {
		if i >= 100 || time.Since(f.at) > 7*24*time.Hour {
			_ = os.Remove(filepath.Join(dir, f.name))
		}
	}
}

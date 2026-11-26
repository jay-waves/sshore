package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/jsonstream"
	"github.com/jay-waves/sshore/internal/sys"
)

func requestDaemonShutdown(expected daemonSnapshot) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	record, err := existingDaemonRecord()
	if err != nil {
		return err
	}
	if record.identity() != expected.Instance {
		return fmt.Errorf("daemon changed; review again")
	}
	dir, err := daemonLocation()
	if err != nil {
		return err
	}
	return sys.WaitForDaemonShutdown(ctx, record.ManagerPID, func() error {
		return shutdownDaemonEndpoint(ctx, dir, record, expected)
	})
}

func shutdownDaemonEndpoint(ctx context.Context, dir string, record localShellRecord, expected daemonSnapshot) error {
	// The account lock file survives shutdown and reset. Keep its identity
	// until ownership is released, then close it before any process-exit wait.
	owner, err := openDaemonOwner(dir)
	if err != nil {
		return err
	}
	defer owner.Close()
	if _, err := daemonAdminRequest(ctx, record, daemonShutdownRoute, &expected); err != nil {
		return err
	}
	recordPath := filepath.Join(dir, "daemon.json")
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		lockErr := owner.TryLock()
		if lockErr == nil {
			return nil
		}
		if !errors.Is(lockErr, sys.ErrFileLockBusy) {
			return lockErr
		}
		// Locator removal precedes owner release; absence alone is not exit.
		r, err := readLocalRecord(recordPath, "daemon")
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && r.identity() != expected.Instance {
			return fmt.Errorf("daemon changed during shutdown; review again")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("daemon did not release its account endpoint")
		case <-tick.C:
		}
	}
}

// Administrative control can inspect or stop a different release, but cannot
// create tasks or open data streams. Normal control still requires exact versions.
type daemonTaskInfo struct{ ID, Name string }
type daemonSnapshot struct {
	Version, Instance string
	Tasks             []daemonTaskInfo
}
type daemonAdminReply struct {
	Snapshot daemonSnapshot
	Error    string `json:",omitempty"`
}

func (s *daemonTasks) adminSnapshot(record localShellRecord) daemonAdminReply {
	snapshot := daemonSnapshot{Version: record.Version, Instance: record.identity()}
	s.mu.Lock()
	for id, task := range s.liveLocked() {
		snapshot.Tasks = append(snapshot.Tasks, daemonTaskInfo{ID: id, Name: task.taskName()})
	}
	s.mu.Unlock()
	slices.SortFunc(snapshot.Tasks, func(a, b daemonTaskInfo) int { return cmp.Compare(a.ID, b.ID) })
	return daemonAdminReply{Snapshot: snapshot}
}

func readDaemonSnapshot(ctx context.Context) (daemonSnapshot, error) {
	r, err := existingDaemonRecord()
	if err != nil {
		return daemonSnapshot{}, err
	}
	return daemonAdminRequest(ctx, r, daemonSnapshotRoute, nil)
}

func daemonAdminRequest(ctx context.Context, r localShellRecord, route string, expected *daemonSnapshot) (daemonSnapshot, error) {
	var reply daemonAdminReply
	if err := daemonJSON(ctx, r, route, expected, &reply); err != nil {
		return daemonSnapshot{}, err
	}
	if reply.Error != "" {
		return daemonSnapshot{}, fmt.Errorf("%s", reply.Error)
	}
	return reply.Snapshot, nil
}

func (s *daemonTasks) beginShutdown(record localShellRecord, expected daemonSnapshot) ([]daemonTask, error) {
	changed := errors.New("tasks changed; review again")
	if record.Version != expected.Version || record.identity() != expected.Instance {
		return nil, changed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if len(s.records) != len(expected.Tasks) {
		return nil, changed
	}
	var stops []daemonTask
	previousID := ""
	for _, entry := range expected.Tasks {
		if entry.ID <= previousID {
			return nil, changed
		}
		previousID = entry.ID
		task := s.records[entry.ID]
		if task == nil || task.taskName() != entry.Name {
			return nil, changed
		}
		stops = append(stops, task)
	}
	if s.draining {
		return nil, fmt.Errorf("daemon is already shutting down")
	}
	s.draining = true
	return stops, nil
}

func (s *daemonTasks) resumeAdmission() {
	s.mu.Lock()
	s.draining = false
	s.mu.Unlock()
}

func (s *daemonTasks) isShuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

func (s *daemonTasks) shutdown(record localShellRecord, expected daemonSnapshot) error {
	stops, err := s.beginShutdown(record, expected)
	if err != nil {
		return err
	}
	if len(stops) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	result := make(chan error)
	go func() {
		var workers sync.WaitGroup
		failures := make([]error, len(stops))
		for i, task := range stops {
			workers.Go(func() {
				if err := task.stopTask(); err != nil {
					failures[i] = fmt.Errorf("%s: %w", cleanText(task.taskName()), err)
				}
			})
		}
		workers.Wait()
		err := errors.Join(failures...)
		if err != nil {
			s.resumeAdmission()
		}
		select {
		case result <- err:
		case <-ctx.Done():
			// The caller timed out. Admission stays closed until all stops finish.
			if err == nil {
				s.resumeAdmission()
			}
		}
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return fmt.Errorf("daemon task cleanup timed out; shutdown stopped")
	}
}
func (t *nativeShellTask) stopTask() error { return t.shell.stopTask() }

// An SSH exec channel exposes only the authenticated account's admin exchange.
// Waiting for confirmation never grants the helper permission to start tasks.
func serveDaemonAdmin(input io.Reader, output io.Writer) error {
	snapshot, err := readDaemonSnapshot(context.Background())
	if os.IsNotExist(err) {
		err = nil
	}
	reply := daemonAdminReply{Snapshot: snapshot}
	if err != nil {
		reply.Error = err.Error()
	}
	encoder := jsonstream.NewEncoder(output)
	if err := encoder.Encode(reply); err != nil {
		return err
	}
	if reply.Error != "" {
		return nil
	}
	var expected daemonSnapshot
	if err := jsonstream.NewDecoder(io.LimitReader(input, 1<<20)).Decode(&expected); err != nil {
		return err
	}
	if snapshot.Instance != expected.Instance || snapshot.Version != expected.Version || !slices.Equal(snapshot.Tasks, expected.Tasks) {
		return encoder.Encode(daemonAdminReply{Error: "tasks changed; review again"})
	}
	if snapshot.Instance != "" {
		err = requestDaemonShutdown(expected)
	}
	reply = daemonAdminReply{}
	if err != nil {
		reply.Error = err.Error()
	}
	return encoder.Encode(reply)
}

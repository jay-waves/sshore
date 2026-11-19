package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// RPC fixtures must never start or acquire the developer's account daemon.
func startRequestDaemon(t *testing.T, release ...string) *daemonTasks {
	t.Helper()
	activityFixture(t)
	expected, err := statePath("daemon")
	if err != nil {
		t.Skip("isolated daemon scope unavailable:", err)
	}
	actual, err := daemonLocation()
	if err != nil || filepath.Clean(expected) != filepath.Clean(actual) {
		t.Skip("request RPC tests require the isolated test daemon scope")
	}
	record, err := newDaemonRecord()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := daemonFileLock("owner")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		owner.Close()
		t.Fatal(err)
	}
	if len(release) > 0 {
		record.Version = release[0]
	}
	record.ManagerPID, record.Address = os.Getpid(), listener.Addr().String()
	path, err := daemonPath()
	if err != nil {
		listener.Close()
		owner.Close()
		t.Fatal(err)
	}
	if err := writeStateJSON(path, record); err != nil {
		listener.Close()
		owner.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	tasks := &daemonTasks{}
	accountActivity.Load().tasks = tasks
	services := newDaemonServices(tasks)
	if services != nil {
		t.Cleanup(services.Close)
	}
	go func() { done <- serveDaemon(listener, record, services, tasks) }()
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-done:
		case <-time.After(requestTimeout):
			t.Error("request daemon did not exit")
		}
		_ = os.Remove(path)
		owner.Close()
	})
	return tasks
}

func pendingRequestFixture(t *testing.T) requestRecord {
	t.Helper()
	return requestRecord{
		ID: strings.Repeat("a", 32), Name: "request", PID: 123,
		SoftwareVersion: version, Created: time.Now(), Directory: t.TempDir(),
		Connection: recentConnection{Host: "work", Session: "source", Server: "sshore", Instance: strings.Repeat("b", 32)},
		Operations: []operation{{Kind: "download", Path: "/tmp/a", Name: "a", Size: 1}},
	}
}

func TestPendingRequestsReturnIndependentCopies(t *testing.T) {
	s := &daemonTasks{}
	r := pendingRequestFixture(t)
	r.Connection.Parent = &recentConnection{Host: "entry", Session: "parent", Server: "sshore", Instance: strings.Repeat("c", 32), SSH: []string{"-p", "2222"}}
	r.Connection.RouteToken = strings.Repeat("d", 64)
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	r.Connection.Parent.SSH[1], r.Operations[0].Path = "3333", "/changed"
	copy, err := s.Find(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if copy.Connection.Parent.SSH[1] != "2222" || copy.Operations[0].Path != "/tmp/a" {
		t.Fatalf("pending intent retained mutable caller state: %+v", copy)
	}
	copy.Connection.Parent.SSH[1], copy.Operations[0].Path = "4444", "/approved-copy"
	listed := s.Pending()
	if len(listed) != 1 || listed[0].Connection.Parent.SSH[1] != "2222" || listed[0].Operations[0].Path != "/tmp/a" {
		t.Fatalf("review edits changed stored intent: %+v", listed)
	}
	listed[0].Operations[0].Path = "/list-copy"
	if found, err := s.Find(r.Name); err != nil || found.Operations[0].Path != "/tmp/a" {
		t.Fatal(found, err)
	}
}

func TestPendingRequestConsumptionIsAtomic(t *testing.T) {
	s := &daemonTasks{}
	r := pendingRequestFixture(t)
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	var consumed atomic.Int32
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			if s.Consume(r.ID) == nil {
				consumed.Add(1)
			}
		})
	}
	workers.Wait()
	if consumed.Load() != 1 || s.Busy() {
		t.Fatalf("consumed %d times", consumed.Load())
	}
	if _, err := s.Find(r.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("consumed request survived:", err)
	}
}

func TestPendingRequestCapacityAndExpiration(t *testing.T) {
	s := &daemonTasks{}
	r := pendingRequestFixture(t)
	var saved atomic.Int32
	var workers sync.WaitGroup
	for i := range maxPendingRequests + 8 {
		workers.Go(func() {
			copy := r
			copy.ID, copy.Name = fmt.Sprintf("%032x", i+1), fmt.Sprintf("request-%d", i)
			if s.Save(copy) == nil {
				saved.Add(1)
			}
		})
	}
	workers.Wait()
	if saved.Load() != maxPendingRequests || len(s.Pending()) != maxPendingRequests {
		t.Fatalf("capacity was not atomic: %d", saved.Load())
	}
	// Simulate clock passage without sleeping through the ten-minute TTL.
	for id, task := range s.records {
		record := task.(requestRecord)
		record.Created = time.Now().Add(-requestTTL - time.Second)
		s.records[id] = record
	}
	if s.Busy() || len(s.Pending()) != 0 {
		t.Fatal("expired requests kept the daemon alive")
	}
	if err := s.Save(r); err != nil {
		t.Fatal("expired requests retained capacity:", err)
	}
}

func TestPendingRequestRenameAndValidation(t *testing.T) {
	s := &daemonTasks{}
	r := pendingRequestFixture(t)
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	other := r
	other.ID, other.Name = strings.Repeat("c", 32), "other"
	if err := s.Save(other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.mutateResource("rename", resourceMutation{Kind: "review", Ref: r.ID, NewName: other.Name}); !errors.Is(err, errObjectNameInUse) {
		t.Fatal("rename took another pending name:", err)
	}
	if _, _, err := s.mutateResource("rename", resourceMutation{Kind: "review", Ref: r.ID, NewName: "renamed"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Find("renamed"); err != nil || got.ID != r.ID {
		t.Fatal(got, err)
	}
	if err := s.Save(r); err == nil {
		t.Fatal("pending request was overwritten")
	}
	for _, fault := range []string{"release", "identity", "expired", "size"} {
		bad := pendingRequestFixture(t)
		switch fault {
		case "release":
			bad.SoftwareVersion = "0.0.0"
		case "identity":
			bad.Connection.Instance = ""
		case "expired":
			bad.Created = time.Now().Add(-requestTTL - time.Second)
		case "size":
			bad.DownloadDir = strings.Repeat("x", maxRequestRecord)
		}
		if err := (&daemonTasks{}).Save(bad); err == nil {
			t.Fatal("accepted invalid", fault)
		}
	}
}

func TestRequestRPCDoesNotCreatePersistentApprovalFiles(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	if pending, err := pendingRequests(); err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	locator, _ := daemonLocation()
	if _, err := os.Stat(locator); !os.IsNotExist(err) {
		t.Fatal("reading pending requests created daemon state")
	}
	startRequestDaemon(t)
	r := pendingRequestFixture(t)
	if err := saveRequest(r); err != nil {
		t.Fatal(err)
	}
	got, err := findRequest(r.Name)
	if err != nil || got.ID != r.ID {
		t.Fatal(got, err)
	}
	if _, err := runRenameResource("review", r.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := consumeRequest(r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := findRequest(r.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("RPC restored a consumed request:", err)
	}
	files, _ := statePath("approvals")
	if _, err := os.Stat(files); !os.IsNotExist(err) {
		t.Fatal("request RPC created approval files")
	}
}

func TestRequestRPCPreservesLargeBoundedRequests(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	startRequestDaemon(t)
	r := pendingRequestFixture(t)
	r.Operations = nil
	for i := range maxRequestItems {
		r.Operations = append(r.Operations, operation{Kind: "upload", Path: "/tmp/" + strings.Repeat("x", 4070) + fmt.Sprint(i), LocalHint: strings.Repeat("p", 2000)})
	}
	if _, err := requestRPC("save", "", &r); err != nil {
		t.Fatal(err)
	}
	got, err := requestRPC("get", r.ID, nil)
	copy := got.Request
	if err != nil || copy == nil || len(copy.Operations) != maxRequestItems || copy.Operations[15].Path != r.Operations[15].Path {
		t.Fatalf("large request lost metadata: %v", err)
	}
	reply, err := requestRPC("list", "", nil)
	listed := reply.Requests
	if err != nil || len(listed) != 1 || len(listed[0].Operations[0].LocalHint) != 2000 {
		t.Fatalf("bounded request list lost metadata: %v", err)
	}
}

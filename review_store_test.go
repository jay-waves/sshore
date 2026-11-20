package main

import (
	"net"
	"os"
	"path/filepath"
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
	go func() { done <- serveDaemon(listener, record, tasks) }()
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

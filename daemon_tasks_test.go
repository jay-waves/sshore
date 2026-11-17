package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func registerTestShell(t *testing.T, tasks *daemonTasks, name, id string) localShellRecord {
	t.Helper()
	r, err := existingDaemonRecord()
	if err != nil {
		t.Fatal(err)
	}
	r.Name, r.UID = name, id
	s := &localShell{record: r, tasks: tasks, done: make(chan struct{})}
	task := &nativeShellTask{shell: s, serve: func(c net.Conn, m message) error {
		return writeMessage(c, message{Type: "session", Name: name, Instance: id})
	}}
	if err := tasks.register(id, task); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDaemonTaskConnectionsDoNotOwnLifetime(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	tasks := startRequestDaemon(t)
	r := registerTestShell(t, tasks, "work", strings.Repeat("c", 32))
	for range 2 {
		found, err := readLocalSession(r.UID)
		if err != nil {
			t.Fatal(err)
		}
		c, err := dialLocalSession(found)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeMessage(c, message{Type: "status"}); err != nil {
			t.Fatal(err)
		}
		m, err := readMessage(c)
		c.Close()
		if err != nil || m.Instance != r.UID {
			t.Fatalf("task route: %+v %v", m, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "local-shells")); !os.IsNotExist(err) {
		t.Fatalf("shell routing created state files: %v", err)
	}
	// Ending the task invalidates its identity even when its name is reused.
	tasks.mu.Lock()
	close(tasks.records[r.UID].(*nativeShellTask).shell.done)
	tasks.mu.Unlock()
	registerTestShell(t, tasks, "work", strings.Repeat("d", 32))
	if _, err := readLocalSession(r.UID); !os.IsNotExist(err) {
		t.Fatalf("ended ID resolved: %v", err)
	}
	if c, err := dialLocalSession(r); err == nil {
		c.Close()
		t.Fatal("ended ID opened replacement")
	}
}

func TestDaemonTaskNamesSharedWithRequests(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	registerTestShell(t, tasks, "work", strings.Repeat("c", 32))
	r := pendingRequestFixture(t)
	r.Name = "work"
	if err := tasks.Save(r); err == nil {
		t.Fatal("request reused a live shell name")
	}
	r.Name = "request"
	if err := tasks.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := tasks.Rename(r.ID, "work"); err == nil {
		t.Fatal("request renamed over a shell")
	}
	if err := tasks.Consume(strings.Repeat("c", 32)); err == nil {
		t.Fatal("request consumption removed a shell")
	}
}

func registerTestResource(t *testing.T, tasks *daemonTasks, kind, name, id string) (*daemonResourceTask, chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	r := resourceRecord{UID: id, Kind: kind, Name: name, Host: "work", Version: version}
	task := &daemonResourceTask{record: r, done: done, control: func(action string) resourceReply {
		if action == "kill" {
			close(done)
		}
		return resourceReply{Status: resourceStatus{Version: version, Transport: "connected", Target: "unchecked"}}
	}}
	if err := tasks.register(id, task); err != nil {
		t.Fatal(err)
	}
	return task, done
}

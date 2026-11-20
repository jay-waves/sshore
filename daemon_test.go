package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonRoutesAfterMCPRemoval(t *testing.T) {
	mux := daemonRoutes(localShellRecord{}, &daemonTasks{}, func() {})
	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/ready", want: http.StatusNoContent},
		{path: "/mcp", want: http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, tc.path, nil))
		if recorder.Code != tc.want {
			t.Errorf("POST %s: got %d, want %d", tc.path, recorder.Code, tc.want)
		}
		if tc.want == http.StatusNoContent && recorder.Body.Len() != 0 {
			t.Errorf("POST %s: unexpected response body %q", tc.path, recorder.Body.String())
		}
	}
}

func registerTestShell(t *testing.T, tasks *daemonTasks, name, id string) localShellRecord {
	t.Helper()
	r, err := existingDaemonRecord()
	if err != nil {
		t.Fatal(err)
	}
	r.Name, r.UID = name, id
	s := &localShell{shellTask: shellTask{record: r, tasks: tasks, done: make(chan struct{})}}
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
	if _, _, err := openNativeShell(context.Background(), r.Name, "invalid"); !os.IsNotExist(err) {
		t.Fatalf("invalid identity resolved by name: %v", err)
	}
	for range 2 {
		c, _, err := openNativeShell(context.Background(), r.UID, "")
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
	if _, _, err := openNativeShell(context.Background(), r.UID, ""); !os.IsNotExist(err) {
		t.Fatalf("ended ID resolved: %v", err)
	}
	if c, err := dialLocalSessionContext(context.Background(), r, time.Now().Add(5*time.Second)); err == nil {
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
	if _, _, err := tasks.mutateResource("rename", resourceMutation{Kind: "review", Ref: r.ID, NewName: "work"}); err == nil {
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

func TestDaemonShutdownRequiresCurrentSnapshot(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	_, firstDone := registerTestResource(t, tasks, "forward", "first", strings.Repeat("a", 32))
	snapshot, err := daemonAdmin(context.Background(), "snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	registerTestResource(t, tasks, "mount", "second", strings.Repeat("b", 32))
	if _, err := daemonAdmin(context.Background(), "shutdown", &snapshot); err == nil {
		t.Fatal("stale confirmation stopped tasks")
	}
	select {
	case <-firstDone:
		t.Fatal("stale request ended a task")
	default:
	}
	snapshot, err = daemonAdmin(context.Background(), "snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemonAdmin(context.Background(), "shutdown", &snapshot); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstDone:
	default:
		t.Fatal("confirmed shutdown did not stop task")
	}
}

func TestDaemonShutdownFailureKeepsControlAvailable(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	task, done := registerTestResource(t, tasks, "mount", "busy", strings.Repeat("a", 32))
	task.control = func(string) resourceReply { return resourceReply{Error: "mount is busy"} }
	snapshot, err := daemonAdmin(context.Background(), "snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemonAdmin(context.Background(), "shutdown", &snapshot); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("refusal: %v", err)
	}
	select {
	case <-done:
		t.Fatal("refusing task was forced to exit")
	default:
	}
	current, err := daemonAdmin(context.Background(), "snapshot", nil)
	if err != nil || len(current.Tasks) != 1 || current.Tasks[0].ID != task.record.UID {
		t.Fatalf("lost live task: %+v %v", current, err)
	}
}

func TestOwnedDaemonWithoutLocatorIsNotAbsent(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	startRequestDaemon(t)
	path, err := daemonPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := existingDaemonRecord(); err == nil || os.IsNotExist(err) {
		t.Fatalf("live owner treated as absent: %v", err)
	}
}

func TestDaemonAdministrationAcrossReleases(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t, "0.0.1")
	_, done := registerTestResource(t, tasks, "forward", "older", strings.Repeat("a", 32))
	if _, err := nativeShells(); err == nil {
		t.Fatal("normal control accepted a different release")
	}
	snapshot, err := daemonAdmin(context.Background(), "snapshot", nil)
	if err != nil || snapshot.Version != "0.0.1" || len(snapshot.Tasks) != 1 {
		t.Fatalf("cross-release snapshot: %+v %v", snapshot, err)
	}
	if _, err := daemonAdmin(context.Background(), "shutdown", &snapshot); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("cross-release shutdown did not stop the confirmed task")
	}
}

func TestDaemonStateRootFollowsOnlyLiveOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "daemon")
	requested, active := t.TempDir(), t.TempDir()
	if root, err := daemonStateRootAt(dir, requested, false); err != nil || root != requested {
		t.Fatalf("missing daemon changed scope: %s %v", root, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("path lookup created daemon state")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := lockResourceState(owner); err != nil {
		t.Fatal(err)
	}
	record := localShellRecord{Name: "daemon", Version: version, Token: resourceToken(), Address: "127.0.0.1:1234", Directory: active}
	if err := writeStateJSON(filepath.Join(dir, "daemon.json"), record); err != nil {
		t.Fatal(err)
	}
	if root, err := daemonStateRootAt(dir, requested, false); err != nil || root != active {
		t.Fatalf("default client lost live scope: %s %v", root, err)
	}
	if _, err := daemonStateRootAt(dir, requested, true); err == nil {
		t.Fatal("conflicting override accepted")
	}
	if root, err := daemonStateRootAt(dir, active, true); err != nil || root != active {
		t.Fatalf("same override rejected: %s %v", root, err)
	}
	owner.Close()
	if root, err := daemonStateRootAt(dir, requested, false); err != nil || root != requested {
		t.Fatalf("stale locator pinned scope: %s %v", root, err)
	}
}

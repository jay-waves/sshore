package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestDaemonListsLocalTasksAndRecordedShells(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	registerTestResource(t, tasks, "forward", "web", strings.Repeat("a", 32))
	activity := accountActivity.Load()
	for _, host := range []string{"work", "home"} {
		row := participatingShell{
			Host: host, Instance: newTaskUID(), Name: "build", State: taskDetached,
			PID: 1234, Attachment: newTaskUID(),
		}
		if err := observeParticipatingShell(row); err != nil {
			t.Fatal(err)
		}
	}
	stale := testPeerTask()
	stop := beginIncomingTask(stale)
	stop()
	active := stale
	active.Key = strings.Repeat("d", 32)
	lease, err := activity.registerLease(leaseRequest{Kind: "incoming", Peer: &active})
	if err != nil {
		t.Fatal(err)
	}
	defer activity.remove(lease)
	record, err := existingDaemonRecord()
	if err != nil {
		t.Fatal(err)
	}
	var local inventory
	if err := daemonJSON(context.Background(), record, "/tasks/list", taskListRequest{Native: true}, &local); err != nil {
		t.Fatal(err)
	}
	if len(local.Shells) != 0 {
		t.Fatalf("remote observations visible in local list: %+v", local.Shells)
	}
	if len(local.Peers) != 1 || local.Peers[0].Identity != active.Key {
		t.Fatalf("list included stale peer or omitted an active incoming resource: %+v", local.Peers)
	}
	var combined inventory
	if err := daemonJSON(context.Background(), record, "/tasks/list", taskListRequest{Native: true, Observed: true}, &combined); err != nil {
		t.Fatal(err)
	}
	if len(combined.Resources) != 1 || len(combined.Peers) != 1 || len(combined.Shells) != 2 {
		t.Fatalf("incomplete inventory: %+v", combined)
	}
	for i, host := range []string{"work", "home"} {
		shell := combined.Shells[i]
		if shell.Host != host || !shell.Observed || shell.State != taskDetached || shell.ShortID == "" || shell.SeenAt.IsZero() {
			t.Fatalf("recorded shell: %+v", shell)
		}
	}
	var rendered strings.Builder
	writeTaskInventory(&rendered, combined)
	for _, want := range []string{"HOST  local", "HOST  work", "HOST  home", "seen just now"} {
		if !strings.Contains(rendered.String(), want) {
			t.Fatalf("missing %q: %s", want, rendered.String())
		}
	}
}

func TestTaskTreeMetadataWithinDaemon(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	activityFixture(t)
	if _, err := metadataRPC(metadataRequest{Action: "tree_save", Key: "child", Name: "child", Tree: []shellTreeNode{{Identity: "parent", State: taskActive}}, State: taskActive}); err != nil {
		t.Fatal(err)
	}
	reply, err := metadataRPC(metadataRequest{Action: "tree_get", Key: "child"})
	if err != nil || len(reply.Tree) != 2 || reply.Tree[0].Identity != "parent" || reply.Tree[1].Identity != "child" {
		t.Fatalf("tree metadata: %+v, %v", reply.Tree, err)
	}
}

func TestTaskTreeDaemonSnapshot(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	first := registerTestShell(t, tasks, "first", strings.Repeat("a", 32)).identity()
	second := registerTestShell(t, tasks, "second", strings.Repeat("b", 32)).identity()
	saveShellTree(first, "first", []shellTreeNode{{Identity: second, State: taskActive}}, taskActive, "parent")
	saveShellTree(second, "second", []shellTreeNode{{Identity: first, State: taskActive}}, taskActive, "parent")
	saveShellTree("cache-only", "cache-only", nil, taskDetached, "persistent")
	for _, state := range []string{taskActive, taskDetached} {
		err := observeParticipatingShell(participatingShell{
			Host: "work", Instance: newTaskUID(), Name: state, State: state,
			PID: 1234, Attachment: newTaskUID(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	snapshot, _, err := readTaskInventory(taskInventoryRequest{Detail: "tree"})
	if err != nil || snapshot.Error != "" {
		t.Fatalf("tree snapshot: %+v, %v", snapshot, err)
	}
	rows := snapshot.Tree
	if len(rows) != 3 || rows[0].Identity != first || rows[0].Owner != 0 || rows[1].Identity != second || rows[1].Owner != 0 {
		t.Fatalf("cached ownership changed the registered tree: %+v", rows)
	}
	if row := rows[2]; row.Owner != 0 || row.Remote != "work" || row.Code == "" || row.State != taskDetached || row.SeenAt.IsZero() {
		t.Fatalf("recorded detach: %+v", row)
	}
	bin := testBinary(t)
	for _, current := range []string{first, second} {
		t.Run(current, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(bin, "pstree")
			cmd.Env = append(os.Environ(), "SSHORE_TREE_ID="+current, "NO_COLOR=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("pstree: %s (%v)", out, err)
			}
			text := strings.TrimSpace(string(out))
			console := consoleInventory(message{Name: "pstree"}, current)
			if text != console.Output {
				t.Fatalf("standalone tree %q, console tree %q", text, console.Output)
			}
			want := "(first)  [starting]  *"
			if current == second {
				want = "(second)  [starting]  *"
			}
			if strings.Count(text, "  *") != 1 || !strings.Contains(text, want) {
				t.Fatal(text)
			}
			if !strings.HasSuffix(text, "sh/"+rows[2].Code+" (detached)  [detached]  -> work  seen just now") {
				t.Fatalf("recorded detach must be the last root: %q", text)
			}
		})
	}
}

func TestTaskTreeSnapshotUnlocksBeforeStatus(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	id := strings.Repeat("c", 32)
	task := &daemonResourceTask{record: resourceRecord{UID: id, Kind: "forward", Name: "worker", Version: version},
		done: make(chan struct{}), control: func(string) resourceReply {
			close(started)
			<-release
			return resourceReply{}
		}}
	if err := tasks.register(id, task); err != nil {
		t.Fatal(err)
	}

	first := make(chan []taskTreeRow, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	activity := accountActivity.Load()
	go func() {
		rows, err := activity.taskTreeSnapshot(ctx)
		if err != nil {
			t.Error(err)
		}
		first <- rows
	}()
	select {
	case <-started:
	case <-time.After(requestTimeout):
		t.Fatal("status probe did not start")
	}

	shell := registerTestShell(t, tasks, "new", strings.Repeat("d", 32))
	second, _, err := readTaskInventory(taskInventoryRequest{Detail: "tree"})
	if err != nil || second.Error != "" || len(second.Tree) != 2 || second.Tree[1].Identity != shell.identity() {
		t.Fatalf("second snapshot: %+v, %v", second.Tree, err)
	}
	cancel()
	select {
	case snapshot := <-first:
		if len(snapshot) != 1 || snapshot[0].Identity != "task:"+id {
			t.Fatalf("first snapshot changed after registration: %+v", snapshot)
		}
	case <-time.After(requestTimeout):
		t.Fatal("snapshot did not finish after cancel")
	}
}

func TestTaskTreeDaemonUnavailable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	bin := testBinary(t)
	out, err := exec.Command(bin, "pstree").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "No known tasks." {
		t.Fatalf("pstree without daemon: %s (%v)", out, err)
	}
	if _, err := os.Stat(filepath.Join(root, "daemon")); !os.IsNotExist(err) {
		t.Fatalf("read created a daemon locator directory: %v", err)
	}

	startRequestDaemon(t)
	path, err := daemonPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err = exec.Command(bin, "pstree").CombinedOutput()
	if err == nil || strings.Contains(string(out), "No known tasks.") || !strings.Contains(string(out), "read account daemon endpoint") ||
		!strings.Contains(string(out), "daemon.json") || !strings.Contains(string(out), "unexpected EOF") {
		t.Fatalf("pstree with corrupt daemon endpoint: %s (%v)", out, err)
	}
}

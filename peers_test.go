package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func activityFixture(t *testing.T) *daemonActivity {
	t.Helper()
	state := &daemonActivity{}
	previous := accountActivity.Swap(state)
	t.Cleanup(func() { accountActivity.Store(previous) })
	return state
}
func testPeerTask() peerRecord {
	return peerRecord{Version: version, Key: strings.Repeat("a", 32), ID: "forward/web", Manager: "test-client", State: "active", Detail: "server 8080 -> manager 3000"}
}
func TestPeerTaskPrivacyAndStorage(t *testing.T) {
	activityFixture(t)
	task := testPeerTask()
	if err := storeRemoteTask(task); err != nil {
		t.Fatal(err)
	}
	rows, err := collectPeerRows()
	if err != nil || len(rows) != 1 || rows[0].task != task {
		t.Fatal(rows, err)
	}
	if removeRemoteTask("../outside") == nil {
		t.Fatal("accepted invalid key")
	}
	if err := removeRemoteTask(task.Key); err != nil {
		t.Fatal(err)
	}
	rows, err = collectPeerRows()
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	service := &resourceService{record: resourceRecord{Kind: "mount", Name: "work", Remote: "/project", Mountpoint: "/private/local/path", Address: "127.0.0.1:9999", Token: "secret-token", Host: "private-alias"}}
	data, _ := json.Marshal(service.peerTask())
	for _, secret := range []string{"/private/local/path", "secret-token", "9999", "private-alias"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("private data published", string(data))
		}
	}
	task.Version = "older"
	if storeRemoteTask(task) == nil {
		t.Fatal("accepted incompatible version")
	}
}
func TestPeerTaskLeaseStates(t *testing.T) {
	state := activityFixture(t)
	task := testPeerTask()
	if err := storeRemoteTask(task); err != nil {
		t.Fatal(err)
	}
	state.peers[task.Key] = peerAdvertisement{Record: task, Seen: time.Now().Add(-2 * time.Minute)}
	rows, err := collectPeerRows()
	if err != nil || len(rows) != 1 || rows[0].state != "unknown" {
		t.Fatal(rows, err)
	}
	state.peers[task.Key] = peerAdvertisement{Record: task, Seen: time.Now().Add(-25 * time.Hour)}
	rows, err = collectPeerRows()
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
func TestIncomingTaskLifetime(t *testing.T) {
	activityFixture(t)
	task := testPeerTask()
	task.UID, task.Direction = strings.Repeat("b", 32), "<-"
	stop := beginIncomingTask(task)
	defer stop()
	rows, err := collectIncomingTasks()
	if err != nil || len(rows) != 1 || rows[0].UID != task.UID {
		t.Fatal(rows, err)
	}
	task.ID = "forward/new-alias"
	if err := storeRemoteTask(task); err != nil {
		t.Fatal(err)
	}
	rows, err = collectIncomingTasks()
	if err != nil || len(rows) != 1 || rows[0].ID != task.ID {
		t.Fatal(rows, err)
	}
	stop()
	rows, err = collectIncomingTasks()
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
func TestDaemonActivitySocketLease(t *testing.T) {
	state := &daemonActivity{}
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	task := testPeerTask()
	go func() {
		defer close(done)
		defer server.Close()
		state.serve(server, activityRequest{Kind: "incoming", Peer: &task})
	}()
	client.SetDeadline(time.Now().Add(time.Second))
	ready, err := readMessage(client)
	if err != nil || ready.Type != "activity_ready" {
		t.Fatal(ready, err)
	}
	if len(state.request(activityRequest{Kind: "list"}).Incoming) != 1 {
		t.Fatal("lease missing")
	}
	state.tick(time.Now())
	pulse, err := readMessage(client)
	if err != nil || pulse.Type != "pulse" {
		t.Fatal(pulse, err)
	}
	if err := writeMessage(client, message{Type: "alive"}); err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("EOF did not release lease")
	}
	if len(state.request(activityRequest{Kind: "list"}).Incoming) != 0 {
		t.Fatal("ended lease retained")
	}
}

func TestDaemonMetadataHistoryCannotRecreateLiveState(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	connection, err := trackShellConnection(options{host: "work", session: "shell"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.stop()
	saveShellTree("task", "shell", nil, "attached")
	path, err := connection.snapshot.cachePath()
	if err != nil {
		t.Fatal(err)
	}
	forged := connection.snapshot
	forged.State = "exited"
	if err = writeStateJSON(path, forged); err != nil {
		t.Fatal(err)
	}
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 1 || rows[0].State != "attached" || !rows[0].Active {
		t.Fatal(rows, err)
	}
	// Persisted data is history after a daemon restart, never a live lease.
	if err = writeStateJSON(path, connection.snapshot); err != nil {
		t.Fatal(err)
	}
	activityFixture(t)
	rows, err = readShellSnapshots()
	if err != nil || len(rows) != 1 || rows[0].Active || !rows[0].Unconfirmed {
		t.Fatal(rows, err)
	}
	tree := shellTreeFor("task")
	if len(tree) != 1 || !tree[0].Cached {
		t.Fatal("history became a live task", tree)
	}
}

func TestDaemonResetPreservesLiveOwnership(t *testing.T) {
	state := activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	id := strings.Repeat("c", 32)
	state.tasks = &daemonTasks{}
	shell := &localShell{record: localShellRecord{Name: "shell", UID: id}, done: make(chan struct{})}
	if err := state.tasks.register(id, &nativeShellTask{shell: shell}); err != nil {
		t.Fatal(err)
	}
	saveShellTree("task:"+id, "shell", []shellTreeNode{{Identity: "owner", Name: "parent"}}, "attached", "parent")
	connection, err := trackShellConnection(options{host: "work", session: "remote"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.stop()
	if err = runReset(); err != nil {
		t.Fatal(err)
	}
	tree := shellTreeFor("task:" + id)
	if len(tree) != 2 || tree[1].Owner != "owner" || tree[1].Parent != "owner" {
		t.Fatal("reset changed live ownership", tree)
	}
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 1 || !rows[0].Active {
		t.Fatal("reset lost active lease", rows, err)
	}
}

func TestDaemonClockChecksOwnerIdentity(t *testing.T) {
	state := activityFixture(t)
	lease := state.add(activityRequest{Kind: "owner", Owner: spawnOwner{PID: os.Getpid(), Start: "different-process-identity"}})
	if !state.busy() {
		t.Fatal("owner lease did not retain daemon")
	}
	state.tick(time.Now())
	select {
	case <-lease.expired:
	default:
		t.Fatal("central clock ignored changed owner")
	}
	if state.busy() {
		t.Fatal("expired owner retained daemon")
	}
}

func TestDaemonRejectsBrokenHistoryUntilReset(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	if _, err := stateDir("shells"); err != nil {
		t.Fatal(err)
	}
	path, err := shellCachePath("work", "shell")
	if err != nil {
		t.Fatal(err)
	}
	if err = writeStateFile(path, []byte("invalid history")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = readShellSnapshots(); err == nil {
			t.Fatal("broken history was accepted")
		}
	}
	dir, err := stateDir("connections")
	if err != nil {
		t.Fatal(err)
	}
	if err = writeStateFile(filepath.Join(dir, "recent.json"), []byte("invalid routes")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = readRecentConnections(); err == nil {
			t.Fatal("broken routes were accepted")
		}
	}
	if err = runReset(); err != nil {
		t.Fatal(err)
	}
	if rows, err := readShellSnapshots(); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if rows, err := readRecentConnections(); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}

func TestDaemonLiveMetadataDoesNotReadHistoryFiles(t *testing.T) {
	activityFixture(t)
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	connection, err := trackShellConnection(options{host: "work", session: "shell"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.stop()
	saveShellTree("task", "shell", nil, "attached")
	if err = rememberConnection(options{host: "work", session: "shell", server: "sshore"}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"shells", "shell-tree", "connections"} {
		if err = os.RemoveAll(filepath.Join(root, dir)); err != nil {
			t.Fatal(err)
		}
	}
	if tree := shellTreeFor("task"); len(tree) != 1 || tree[0].State != "attached" {
		t.Fatal(tree)
	}
	if rows, err := readShellSnapshots(); err != nil || len(rows) != 1 || !rows[0].Active {
		t.Fatal(rows, err)
	}
	if rows, err := readRecentConnections(); err != nil || len(rows) != 1 || rows[0].Host != "work" {
		t.Fatal(rows, err)
	}
}

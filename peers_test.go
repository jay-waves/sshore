package main

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func activityFixture(t *testing.T) *daemonActivity {
	t.Helper()
	state := &daemonActivity{tasks: &daemonTasks{}}
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
	rows, err := collectPeerViews()
	if err != nil || len(rows) != 1 || rows[0].ID != "peer/"+task.Key[:8]+"/"+task.ID || rows[0].Identity != task.Key || rows[0].Detail != task.Detail {
		t.Fatal(rows, err)
	}
	if removeRemoteTask("../outside") == nil {
		t.Fatal("accepted invalid key")
	}
	if err := removeRemoteTask(task.Key); err != nil {
		t.Fatal(err)
	}
	rows, err = collectPeerViews()
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
	rows, err := collectPeerViews()
	if err != nil || len(rows) != 1 || rows[0].State != "unknown" {
		t.Fatal(rows, err)
	}
	state.peers[task.Key] = peerAdvertisement{Record: task, Seen: time.Now().Add(-25 * time.Hour)}
	rows, err = collectPeerViews()
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
func TestIncomingTaskLifetime(t *testing.T) {
	state := activityFixture(t)
	task := testPeerTask()
	task.UID, task.Direction = strings.Repeat("b", 32), "<-"
	stop := beginIncomingTask(task)
	defer stop()
	rows := state.request(activityRequest{Kind: "incoming"}).Peers
	if len(rows) != 1 || rows[0].Record.UID != task.UID {
		t.Fatal(rows)
	}
	task.ID = "forward/new-alias"
	if err := storeRemoteTask(task); err != nil {
		t.Fatal(err)
	}
	rows = state.request(activityRequest{Kind: "incoming"}).Peers
	if len(rows) != 1 || rows[0].Record.ID != task.ID {
		t.Fatal(rows)
	}
	stop()
	rows = state.request(activityRequest{Kind: "incoming"}).Peers
	if len(rows) != 0 {
		t.Fatal(rows)
	}
}
func TestDaemonActivitySocketLease(t *testing.T) {
	t.Run("EOF", func(t *testing.T) { testDaemonActivitySocketLease(t, false) })
	t.Run("release", func(t *testing.T) { testDaemonActivitySocketLease(t, true) })
}

func testDaemonActivitySocketLease(t *testing.T, release bool) {
	t.Helper()
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
	if len(state.request(activityRequest{Kind: "incoming"}).Peers) != 1 {
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
	if release {
		if err := writeMessage(client, message{Type: "release"}); err != nil {
			t.Fatal(err)
		}
		if msg, err := readMessage(client); err != io.EOF {
			t.Fatalf("release reply: got %+v, %v, want EOF", msg, err)
		}
	} else {
		client.Close()
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lease did not end")
	}
	if len(state.request(activityRequest{Kind: "incoming"}).Peers) != 0 {
		t.Fatal("ended lease retained")
	}
}

func TestDaemonMetadataHistoryCannotRecreateLiveState(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	saveShellTree("task", "shell", nil, "attached")
	activityFixture(t)
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
	shell := &localShell{shellTask: shellTask{record: localShellRecord{Name: "shell", UID: id}, done: make(chan struct{})}}
	if err := state.tasks.register(id, &nativeShellTask{shell: shell}); err != nil {
		t.Fatal(err)
	}
	saveShellTree("task:"+id, "shell", []shellTreeNode{{Identity: "owner", Name: "parent"}}, "attached", "parent")
	if err := runReset(); err != nil {
		t.Fatal(err)
	}
	tree := shellTreeFor("task:" + id)
	if len(tree) != 2 || tree[1].Owner != "owner" || tree[1].Parent != "owner" {
		t.Fatal("reset changed live ownership", tree)
	}
}

func TestDaemonClockChecksOwnerIdentity(t *testing.T) {
	state := activityFixture(t)
	lease, err := state.registerLease(activityRequest{Kind: "owner", Owner: processIdentity{PID: os.Getpid(), Start: "different-process-identity"}})
	if err != nil {
		t.Fatal(err)
	}
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
	if rows, err := readRecentConnections(); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}

func TestDaemonLiveMetadataDoesNotReadHistoryFiles(t *testing.T) {
	activityFixture(t)
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	saveShellTree("task", "shell", nil, "attached")
	if err := rememberConnection(options{host: "work", session: "shell", server: "sshore"}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"shell-tree", "connections"} {
		if err := os.RemoveAll(filepath.Join(root, dir)); err != nil {
			t.Fatal(err)
		}
	}
	if tree := shellTreeFor("task"); len(tree) != 1 || tree[0].State != "attached" {
		t.Fatal(tree)
	}
	if rows, err := readRecentConnections(); err != nil || len(rows) != 1 || rows[0].Host != "work" {
		t.Fatal(rows, err)
	}
}

func TestSpawnOwnerWatcherLifetime(t *testing.T) {
	state := activityFixture(t)
	synctest.Test(t, func(t *testing.T) {
		owner := processIdentity{PID: os.Getpid(), Start: "obsolete"}
		lost := false
		stop := superviseSpawnOwner(owner, func() { lost = true })
		synctest.Wait()
		if !state.busy() {
			t.Fatal("owner lease missing")
		}
		stop()
		if lost || state.busy() {
			t.Fatal("normal shutdown reported owner loss or leaked its lease")
		}

		entered, release := make(chan struct{}), make(chan struct{})
		stop = superviseSpawnOwner(owner, func() { close(entered); <-release })
		synctest.Wait()
		state.tick(time.Now())
		<-entered
		joined := make(chan struct{})
		go func() { stop(); close(joined) }()
		synctest.Wait()
		select {
		case <-joined:
			t.Fatal("stop returned during owner cleanup")
		default:
		}
		close(release)
		<-joined
		if state.busy() {
			t.Fatal("expired owner lease survived")
		}
	})
}

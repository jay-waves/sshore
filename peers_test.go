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
	return peerRecord{UID: strings.Repeat("b", 32), Direction: "<-", Version: version, Key: strings.Repeat("a", 32), ID: "forward/web", Manager: "test-client", Detail: "manager:3000 <- here:8080"}
}
func TestPeerTaskPrivacyAndRegistration(t *testing.T) {
	state := activityFixture(t)
	task := testPeerTask()
	stop := beginIncomingTask(task)
	defer stop()
	rows := peerViews(state.incomingPeers())
	if len(rows) != 1 || rows[0].ID != "peer/"+task.Key[:8]+"/"+task.ID || rows[0].Identity != task.Key || rows[0].Detail != task.Detail {
		t.Fatal(rows)
	}
	invalid := task
	invalid.Key = "../outside"
	if _, err := state.registerLease(leaseRequest{Kind: "incoming", Peer: &invalid}); err == nil {
		t.Fatal("accepted invalid key")
	}
	service := &resourceService{record: resourceRecord{Kind: "mount", Name: "work", Remote: "/project", Mountpoint: "/private/local/path", Host: "private-alias"}}
	data, _ := json.Marshal(service.mountPeerTask())
	for _, secret := range []string{"/private/local/path", "private-alias"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("private data published", string(data))
		}
	}
	task.Version = "older"
	if _, err := state.registerLease(leaseRequest{Kind: "incoming", Peer: &task}); err == nil {
		t.Fatal("accepted incompatible version")
	}
}
func TestIncomingTaskLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state := activityFixture(t)
		task := testPeerTask()
		older := beginIncomingTask(task)
		input, output := io.Pipe()
		defer input.Close()
		done := make(chan error, 1)
		go func() { done <- watchPeerRecords(input, "forward", nil) }()
		send := func() {
			t.Helper()
			if err := json.NewEncoder(output).Encode(task); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
		}
		send()
		order := state.nextPeerOrder
		send()
		if state.nextPeerOrder != order {
			t.Fatal("identical heartbeat replaced its lease")
		}
		task.ID = "forward/new-alias"
		send()
		checkAlias := func() {
			t.Helper()
			if rows := state.incomingPeers(); len(rows) != 1 || rows[0] != task {
				t.Fatalf("old connection masked new alias: %+v", rows)
			}
		}
		checkAlias()
		older()
		checkAlias()
		output.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if rows := state.incomingPeers(); len(rows) != 0 {
			t.Fatal("closed watcher retained", rows)
		}
	})
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
		state.serve(server, leaseRequest{Kind: "incoming", Peer: &task})
	}()
	client.SetDeadline(time.Now().Add(time.Second))
	ready, err := readMessage(client)
	if err != nil || ready.Type != "activity_ready" {
		t.Fatal(ready, err)
	}
	if len(state.incomingPeers()) != 1 {
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
	if len(state.incomingPeers()) != 0 {
		t.Fatal("ended lease retained")
	}
}

func TestDaemonRemoteObservationDoesNotRegisterShell(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	row := participatingShell{Host: "work", Instance: newTaskUID(), Name: "shell", State: taskActive, PID: 1234, Attachment: newTaskUID()}
	if err := observeParticipatingShell(row); err != nil {
		t.Fatal(err)
	}
	restarted := activityFixture(t)
	inv := restarted.listTasks(t.Context(), taskListRequest{Native: true, Observed: true})
	if restarted.tasks.lookup(row.Instance) != nil || len(inv.Shells) != 1 || !inv.Shells[0].Observed || inv.Shells[0].Instance != row.Instance {
		t.Fatalf("unexpected task registrations or observations after restart: %+v", inv)
	}
}

func TestDaemonRejectsBrokenHistoryUntilReset(t *testing.T) {
	fixture := activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, err := stateDir("connections")
	if err != nil {
		t.Fatal(err)
	}
	if err = writeStateFile(filepath.Join(dir, "recent.json"), []byte("invalid routes")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = fixture.metadata.readRecentConnectionsLocal(); err == nil {
			t.Fatal("broken routes were accepted")
		}
	}
	if err = runReset(); err != nil {
		t.Fatal(err)
	}
	if rows, err := fixture.metadata.readRecentConnectionsLocal(); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}

func TestDaemonDoesNotReloadConnectionHistoryAfterDelete(t *testing.T) {
	fixture := activityFixture(t)
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	if err := rememberConnection(options{host: "work", session: "shell", server: "sshore"}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "connections")); err != nil {
		t.Fatal(err)
	}
	if rows, err := fixture.metadata.readRecentConnectionsLocal(); err != nil || len(rows) != 1 || rows[0].Host != "work" {
		t.Fatal(rows, err)
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInventoryReadsPublishedStateWhileWritersAreBusy(t *testing.T) {
	tasks := &daemonTasks{}
	shell := tasks.newTmuxTask(localShellRecord{Name: "busy-shell"}, nil, nil, false)
	if err := tasks.registerShell(shell, nil); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir, err := prepareStateDir(root, "connections")
	if err != nil {
		t.Fatal(err)
	}
	rows := []participatingShell{{
		Host: "work", Instance: newTaskUID(), Name: "remembered-shell",
		State: taskDetached, PID: 42, Attachment: newTaskUID(),
	}}
	if err := writeStateJSON(filepath.Join(dir, "participation.json"), rows); err != nil {
		t.Fatal(err)
	}
	activity := &daemonActivity{tasks: tasks, root: root}
	activity.metadata.loadParticipation(root)
	defer activity.metadata.closeParticipation()

	// Simulate a metadata writer blocked on disk and a shell busy starting.
	activity.metadata.shellMu.Lock()
	defer activity.metadata.shellMu.Unlock()
	shell.mu.Lock()
	defer shell.mu.Unlock()
	done := make(chan inventory, 1)
	go func() {
		// Shutdown review holds the registry lock while collecting task identity.
		tasks.adminSnapshot(localShellRecord{})
		done <- activity.listTasks()
	}()
	select {
	case snapshot := <-done:
		if len(snapshot.LocalShells) != 1 || snapshot.LocalShells[0].State != taskUnknown {
			t.Fatalf("busy shell missing or mislabeled: %+v", snapshot.LocalShells)
		}
		if len(snapshot.Shells) != 1 || !snapshot.Shells[0].Observed || snapshot.Shells[0].Name != "remembered-shell" {
			t.Fatalf("published history missing: %+v", snapshot.Shells)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inventory blocked on a task or metadata writer")
	}
}

func TestInventoryProbeSkipsDaemonBeforeEndpointPublication(t *testing.T) {
	dir := t.TempDir()
	owner, err := tryPrivateFileLock(filepath.Join(dir, "owner.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// An account can already own the lock while loading its history.
	if _, err := ownedDaemonRecord(dir, 0); !os.IsNotExist(err) {
		t.Fatalf("unpublished daemon endpoint: %v", err)
	}
}

func TestParticipationMigrationForgetAndStaleAttachment(t *testing.T) {
	root := t.TempDir()
	dir, err := prepareStateDir(root, "connections")
	if err != nil {
		t.Fatal(err)
	}
	row := participatingShell{Host: "work", Instance: newTaskUID(), Name: "shell-name", State: taskActive, PID: 42, Attachment: newTaskUID()}
	if err := writeStateJSON(filepath.Join(dir, "participation.json"), []participatingShell{row}); err != nil {
		t.Fatal(err)
	}
	var metadata daemonMetadata
	metadata.loadParticipation(root)
	defer metadata.closeParticipation()
	rows, err := metadata.participatingShells()
	if err != nil || len(rows) != 1 {
		t.Fatalf("legacy history: %+v, %v", rows, err)
	}
	stale := row
	stale.Attachment = newTaskUID()
	stale.State = taskDetached
	if err := metadata.updateParticipation(root, metadataRequest{Action: "participation_state", Participant: &stale}); err != nil {
		t.Fatal(err)
	}
	rows, err = metadata.participatingShells()
	if err != nil || len(rows) != 1 || rows[0].State != taskActive {
		t.Fatalf("stale attachment changed history: %+v, %v", rows, err)
	}
	if err := metadata.updateParticipation(root, metadataRequest{Action: "participation_forget", Host: row.Host, Instance: row.Instance}); err != nil {
		t.Fatal(err)
	}
	metadata.closeParticipation()
	metadata = daemonMetadata{}
	metadata.loadParticipation(root)
	rows, err = metadata.participatingShells()
	if err != nil || len(rows) != 0 {
		t.Fatalf("forgotten legacy history returned after restart: %+v, %v", rows, err)
	}
}

func TestMalformedParticipationHistoryLeavesLiveInventoryAvailable(t *testing.T) {
	root := t.TempDir()
	dir, err := prepareStateDir(root, "connections")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStateFile(filepath.Join(dir, "participation.json"), []byte("{")); err != nil {
		t.Fatal(err)
	}
	activity := &daemonActivity{tasks: &daemonTasks{}, root: root}
	activity.metadata.loadParticipation(root)
	defer activity.metadata.closeParticipation()
	inv := activity.listTasks()
	if len(inv.Errors) != 1 {
		t.Fatalf("history failure not reported: %+v", inv.Errors)
	}
}

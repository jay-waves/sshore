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

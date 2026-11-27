package main

import (
	bolt "go.etcd.io/bbolt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInventoryReadsHistoryWhileShellIsBusy(t *testing.T) {
	tasks := &daemonTasks{}
	shell := tasks.newTmuxTask(localShellRecord{Name: "busy-shell"}, nil, nil, false)
	if err := tasks.registerShell(shell, nil); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "state")
	row := participatingShell{
		Host: "work", Instance: newTaskUID(), Name: "remembered-shell",
		State: taskDetached, PID: 42, Attachment: newTaskUID(),
	}
	activity := &daemonActivity{tasks: tasks, root: root}
	if err := changeParticipation(root, participationChange{Action: "participation_observe", Participant: &row}); err != nil {
		t.Fatal(err)
	}

	// Simulate a shell busy starting.
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
		t.Fatal("inventory blocked on a task")
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

func TestParticipationForgetAndStaleAttachment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	row := participatingShell{Host: "work", Instance: newTaskUID(), Name: "shell-name", State: taskActive, PID: 42, Attachment: newTaskUID()}
	dir, err := prepareStateDir(root, "connections")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStateJSON(filepath.Join(dir, "participation.json"), []participatingShell{row}); err != nil {
		t.Fatal(err)
	}
	if rows, err := testParticipationRows(root); err != nil || len(rows) != 0 {
		t.Fatalf("expected empty database with legacy JSON present: %+v, %v", rows, err)
	}
	if err := changeParticipation(root, participationChange{Action: "participation_observe", Participant: &row}); err != nil {
		t.Fatal(err)
	}
	stale := row
	stale.Attachment = newTaskUID()
	stale.State = taskDetached
	if err := changeParticipation(root, participationChange{Action: "participation_state", Participant: &stale}); err != nil {
		t.Fatal(err)
	}
	rows, err := testParticipationRows(root)
	if err != nil || len(rows) != 1 || rows[0].State != taskActive {
		t.Fatalf("stale attachment changed history: %+v, %v", rows, err)
	}
	if err := changeParticipation(root, participationChange{Action: "participation_forget", Host: row.Host, Instance: row.Instance}); err != nil {
		t.Fatal(err)
	}
	rows, err = testParticipationRows(root)
	if err != nil || len(rows) != 0 {
		t.Fatalf("forgotten observation returned after restart: %+v, %v", rows, err)
	}
}

func TestUnavailableParticipationHistoryLeavesLiveInventoryAvailable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	dir, err := prepareStateDir(root, "connections")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStateFile(filepath.Join(dir, "participation.db"), []byte("invalid database")); err != nil {
		t.Fatal(err)
	}
	tasks := &daemonTasks{}
	shell := tasks.newTmuxTask(localShellRecord{Name: "live-shell"}, nil, nil, false)
	if err := tasks.registerShell(shell, nil); err != nil {
		t.Fatal(err)
	}
	activity := &daemonActivity{tasks: tasks, root: root}
	inv := activity.listTasks()
	if len(inv.LocalShells) != 1 || inv.LocalShells[0].Name != "live-shell" {
		t.Fatalf("live shell missing: %+v", inv.LocalShells)
	}
	if len(inv.Errors) != 1 {
		t.Fatalf("history failure not reported: %+v", inv.Errors)
	}
}

func changeParticipation(root string, change participationChange) error {
	return historyTransaction(root, true, func(tx *bolt.Tx) error {
		return updateParticipation(tx, change)
	})
}

func testParticipationRows(root string) (rows []participatingShell, err error) {
	err = historyTransaction(root, false, func(tx *bolt.Tx) error {
		rows, err = participatingShells(tx)
		return err
	})
	return
}

func TestHistoryResetDiscardsCallbacksFromOldAttachment(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	location := daemonLocation
	daemonLocation = func() (string, error) { return filepath.Join(dir, "daemon"), nil }
	t.Cleanup(func() { daemonLocation = location })

	var oldGeneration string
	if err := updateHistory(func(tx *bolt.Tx) (err error) {
		oldGeneration, err = historyGeneration(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := (resetPaths{state: root, account: filepath.Join(dir, "daemon")}).clear(); err != nil {
		t.Fatal(err)
	}
	if err := updateHistory(func(tx *bolt.Tx) error {
		_, err := historyGeneration(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := updateAttachedHistory(oldGeneration, func(*bolt.Tx) error {
		t.Fatal("callback from an old attachment reached the new database")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

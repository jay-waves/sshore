package main

import (
	bolt "go.etcd.io/bbolt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/taskid"
)

func TestInventoryReadsHistoryWhileShellIsBusy(t *testing.T) {
	tasks := &daemonTasks{}
	shell := tasks.newTmuxTask(localShellRecord{Name: "busy-shell"}, nil, nil, false)
	if err := tasks.registerShell(shell, nil); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "state")
	row := participatingShell{
		Host: "work", Instance: taskid.New(), Name: "remembered-shell",
		State: taskDetached, PID: 42, Attachment: taskid.New(),
	}
	activity := &daemonActivity{tasks: tasks, root: root, instance: taskid.New()}
	if err := historyTransaction(root, true, func(tx *bolt.Tx) error { return observeParticipation(tx, row) }); err != nil {
		t.Fatal(err)
	}

	activity.publishTask(shell)
	// Readers use the last published state even while the shell is busy.
	shell.mu.Lock()
	defer shell.mu.Unlock()
	done := make(chan struct{})
	go func() {
		// Shutdown review holds the registry lock while collecting task identity.
		tasks.adminSnapshot(localShellRecord{})
		close(done)
	}()
	select {
	case <-done:
		snapshot, err := loadInventory(root, activity.instance)
		if err != nil {
			t.Fatal(err)
		}
		offline, err := loadInventory(root, taskid.New())
		if err != nil || len(offline.Shells) != 1 {
			t.Fatalf("stale live view was not discarded: %+v, %v", offline, err)
		}
		if len(snapshot.Shells) != 2 || snapshot.Shells[0].State != taskStarting {
			t.Fatalf("busy shell missing or mislabeled: %+v", snapshot.Shells)
		}
		if !snapshot.Shells[1].Observed || snapshot.Shells[1].Name != "remembered-shell" {
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
	row := participatingShell{Host: "work", Instance: taskid.New(), Name: "shell-name", State: taskActive, PID: 42, Attachment: taskid.New()}
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
	if err := historyTransaction(root, true, func(tx *bolt.Tx) error { return observeParticipation(tx, row) }); err != nil {
		t.Fatal(err)
	}
	stale := row
	stale.Attachment = taskid.New()
	stale.State = taskDetached
	if err := historyTransaction(root, true, func(tx *bolt.Tx) error {
		return setParticipationState(tx, stale)
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := testParticipationRows(root)
	if err != nil || len(rows) != 1 || rows[0].State != taskActive {
		t.Fatalf("stale attachment changed history: %+v, %v", rows, err)
	}
	if err := historyTransaction(root, true, func(tx *bolt.Tx) error { return forgetParticipation(tx, row.Host, row.Instance) }); err != nil {
		t.Fatal(err)
	}
	rows, err = testParticipationRows(root)
	if err != nil || len(rows) != 0 {
		t.Fatalf("forgotten observation returned after restart: %+v, %v", rows, err)
	}
}

func TestUnavailableInventoryReportsDatabaseFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	dir, err := prepareStateDir(root, "connections")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStateFile(filepath.Join(dir, "participation.db"), []byte("invalid database")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInventory(root, taskid.New()); err == nil {
		t.Fatal("inventory reported success with an invalid database")
	}
}

func testParticipationRows(root string) (rows []participatingShell, err error) {
	err = historyTransaction(root, false, func(tx *bolt.Tx) error {
		rows, err = participatingShells(tx)
		return err
	})
	return
}

func TestHistoryResetDeletesDatabase(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := historyTransaction(root, true, func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket(participationBucket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := (resetPaths{state: root, account: filepath.Join(root, "daemon")}).clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "connections", "participation.db")); !os.IsNotExist(err) {
		t.Fatalf("history database after reset: %v", err)
	}
}

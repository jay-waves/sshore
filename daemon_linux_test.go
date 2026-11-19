package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
)

func TestDaemonHostedTaskRouting(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	tasks := startRequestDaemon(t)
	id := strings.Repeat("e", 32)
	selection := taskIdentitySelection{name: "hosted", uid: id}
	if err := createRemoteShell("", selection, false, root, nil); err != nil {
		t.Fatal(err)
	}
	tasks.mu.Lock()
	shell, ok := tasks.records[id].(*remoteShell)
	tasks.mu.Unlock()
	if !ok {
		t.Fatal("creation returned before registering the hosted shell")
	}
	t.Cleanup(func() { _ = shell.stopTask() })
	if err := createRemoteShell("", selection, false, root, nil); err == nil || !strings.Contains(err.Error(), "task already exists") {
		t.Fatalf("duplicate hosted shell creation: %v", err)
	}
	records, err := hostedShells("health")
	if err != nil || len(records) != 1 || records[0].Instance != id {
		t.Fatalf("hosted inventory: %+v %v", records, err)
	}
	for range 2 {
		c, err := dialHostedTask(context.Background(), id, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeMessage(c, message{Type: "status", Action: "health"}); err != nil {
			t.Fatal(err)
		}
		m, err := readMessage(c)
		c.Close()
		if err != nil || m.Instance != id || m.Name != "hosted" {
			t.Fatalf("hosted route: %+v %v", m, err)
		}
	}
	if err := shell.stopTask(); err != nil {
		t.Fatal(err)
	}
	if c, err := dialHostedTask(context.Background(), id, time.Second); err == nil {
		c.Close()
		t.Fatal("ended hosted task accepted a stream")
	}
}

func TestDaemonHangupsDoNotReachChildren(t *testing.T) {
	if os.Getenv("SSHORE_TEST_HANGUP_HELPER") == "1" {
		stop := protectDaemonHangups()
		defer stop()
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		// A caught disposition resets across exec; SIG_IGN would survive and break this.
		cmd := exec.Command("/bin/sh", "-c", "kill -HUP $$; exit 42")
		err := cmd.Run()
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("child did not exit on HUP: %v", err)
		}
		status := exit.Sys().(syscall.WaitStatus)
		if !status.Signaled() || status.Signal() != syscall.SIGHUP {
			t.Fatalf("child inherited ignored HUP: %v", status)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestDaemonHangupsDoNotReachChildren$")
	cmd.Env = append(os.Environ(), "SSHORE_TEST_HANGUP_HELPER=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("daemon signal helper: %v\n%s", err, out)
	}
}

func TestDaemonHangupBetweenHostedShells(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("before-hangup")
	child, err := proc.ReadTask(pid)
	if err != nil {
		t.Fatal(err)
	}
	record, err := existingDaemonRecord()
	if err != nil || record.ManagerPID != child.Parent {
		t.Fatalf("fixture daemon identity: %+v %v", record, err)
	}
	identity, err := proc.StartToken(record.ManagerPID)
	if err != nil {
		t.Fatal(err)
	}
	b.writeInput(t, []byte("exit\n"))
	b.readExit(t)
	b.close()
	waitExitReceipt(t, e.dir, "before-hangup", "shell exited (code 0)")
	if current, err := proc.StartToken(record.ManagerPID); err != nil || current != identity {
		t.Fatalf("fixture daemon changed: %v", err)
	}
	if err := syscall.Kill(record.ManagerPID, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	next, nextPID := e.attach("after-hangup")
	defer next.close()
	nextChild, err := proc.ReadTask(nextPID)
	if err != nil || nextChild.Parent != record.ManagerPID {
		t.Fatalf("HUP replaced account daemon: %+v %v", nextChild, err)
	}
	shellOutput(t, next, "printf 'HUP_%s\\n' survived", "HUP_survived")
}

func TestResetRetainsStateWhenShutdownRefused(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	task, _ := registerTestResource(t, tasks, "mount", "busy", strings.Repeat("a", 32))
	task.control = func(string) resourceReply { return resourceReply{Error: "mount is busy"} }
	root, err := stateDir("history")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "record")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runReset(); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("refusal: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("state lost on refusal: %v", err)
	}
}

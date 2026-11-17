//go:build sshoretest

package main

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

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
	child, err := readSetupTask(pid)
	if err != nil {
		t.Fatal(err)
	}
	record, err := existingDaemonRecord()
	if err != nil || record.ManagerPID != child.parent {
		t.Fatalf("fixture daemon identity: %+v %v", record, err)
	}
	identity, err := processStart(record.ManagerPID)
	if err != nil {
		t.Fatal(err)
	}
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
	b.close()
	waitExitReceipt(t, e.dir, "before-hangup", "shell exited (code 0)")
	if current, err := processStart(record.ManagerPID); err != nil || current != identity {
		t.Fatalf("fixture daemon changed: %v", err)
	}
	if err := syscall.Kill(record.ManagerPID, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	next, nextPID := e.attach("after-hangup")
	defer next.close()
	nextChild, err := readSetupTask(nextPID)
	if err != nil || nextChild.parent != record.ManagerPID {
		t.Fatalf("HUP replaced account daemon: %+v %v", nextChild, err)
	}
	shellOutput(t, next, "printf 'HUP_%s\\n' survived", "HUP_survived")
}

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every lifecycle process uses newEnv's sshoretest-tagged, isolated binary.
func TestSessionMetadataLifecycle(t *testing.T) {
	e := newEnv(t)
	run := func(args ...string) (string, error) {
		cmd := exec.Command(e.bin, args...)
		cmd.Env = e.env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	b, pid := e.attach("original")
	shellOutput(t, b, "cd /tmp; printf 'DIRECTORY_%s\\n' ready", "DIRECTORY_ready")
	if out, err := run("rename-session", "original", "work", "999.0.0"); err == nil {
		t.Fatal("version mismatch accepted", out)
	}
	if out, err := run("rename-session", "original", "work", version); err != nil {
		t.Fatal(out, err)
	}
	if out, err := run("sessions"); err != nil || !strings.Contains(out, "work [attached]") || !strings.Contains(out, "sh · /tmp") || strings.Contains(out, "original [attached]") {
		t.Fatal(out, err)
	}
	sendTest(t, b.tty, message{Type: "input", Data: []byte("sleep 30\n")})
	foregroundSeen := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		out, err := run("sessions")
		if err != nil {
			t.Fatal(out, err)
		}
		if strings.Contains(out, "sleep · /tmp") {
			foregroundSeen = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !foregroundSeen {
		t.Fatal("foreground process or directory missing")
	}
	sendTest(t, b.tty, message{Type: "input", Data: []byte{3}})
	shellOutput(t, b, "printf 'INTERRUPTED_%s\\n' ready", "INTERRUPTED_ready")
	// The helper still uses the original inherited socket after rename.
	helper := exec.Command(e.bin, "detach")
	helper.Env = append(append([]string{}, e.env...), "SSHORE_SOCKET="+filepath.Join(e.dir, "original.sock"))
	if out, err := helper.CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}

	if m := untilType(t, b.out, "detached"); m.Name != "work" {
		t.Fatal("stale detach name", m.Name)
	}
	b.close()
	b, resumed := e.attach("work", "attach")
	if resumed != pid {
		t.Fatal("rename replaced shell")
	}
	other, otherPID := e.attach("original", "new")
	if otherPID == pid {
		t.Fatal("old label not reusable")
	}
	if out, err := run("rename-session", "work", "original", version); err == nil {
		t.Fatal("collision accepted", out)
	}
	shellOutput(t, b, "printf 'RENAMED_%s\\n' alive", "RENAMED_alive")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit 7\n")})
	untilType(t, b.tty, "exit")
	waitExitReceipt(t, e.dir, "work", "shell exited (code 7)")
	if out, err := run("kill-session", "original", version); err != nil {
		t.Fatal(out, err)
	}
	_ = other
	waitExitReceipt(t, e.dir, "original", "cancelled by sshore cancel")
	if out, err := run("sessions"); err != nil || !strings.Contains(out, "Recent exits:") || !strings.Contains(out, "work [ended]") {
		t.Fatal(out, err)
	}
}

func waitExitReceipt(t *testing.T, dir, name, reason string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, record := range readSessionExits(dir) {
			if record.Name == name && record.Reason == reason {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("missing exit receipt %s / %s: %+v", name, reason, readSessionExits(dir))
}

func TestSessionConnectionLossReceipt(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("lost")
	b.close()
	waitExitReceipt(t, e.dir, "lost", "connection lost without detach")
}

func TestSessionExitReceiptBounds(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "test.sock")
	if err := os.Symlink(outside, socket+".exit.json"); err != nil {
		t.Fatal(err)
	}
	saveSessionExit(socket, "test", "shell exited (code 0)")
	if b, _ := os.ReadFile(outside); string(b) != "untouched" {
		t.Fatal("followed symlink")
	}
	if info, err := os.Stat(socket + ".exit.json"); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	for i := 0; i < 102; i++ {
		// Distinct names are not necessary: receipts are keyed by identity.
		saveSessionExit(filepath.Join(dir, time.Unix(int64(i), 0).Format("150405")+".sock"), "test", "ended")
	}
	if got := len(readSessionExits(dir)); got != 100 {
		t.Fatal("unbounded receipts", got)
	}
}

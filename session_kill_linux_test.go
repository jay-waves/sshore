package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Uses the existing isolated -tags=sshoretest executable, never account sessions.
func TestKillSessionLifecycle(t *testing.T) {
	e := newEnv(t)
	query := func(name string) message {
		t.Helper()
		c, err := net.Dial("unix", filepath.Join(e.dir, name+".sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		sendTest(t, c, message{Type: "status"})
		return recv(t, c)
	}
	run := func(name, release string) (string, error) {
		cmd := exec.Command(e.bin, "kill-session", name, release)
		cmd.Env = e.env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	keep, keepPID := e.attach("keep")
	keepProcess, err := readSetupTask(keepPID)
	if err != nil {
		t.Fatal(err)
	}
	blocked := exec.Command(e.bin, "kill-session", "keep", version)
	blocked.Env = append(append([]string{}, e.env...), "SSHORE_DISABLED=1")
	if out, err := blocked.CombinedOutput(); err == nil || !strings.Contains(string(out), "disabled") {
		t.Fatal("recovery allowed kill", string(out), err)
	}
	listing := exec.Command(e.bin, "sessions")
	listing.Env = e.env
	if out, err := listing.CombinedOutput(); err != nil || !strings.Contains(string(out), "keep [attached] · started ") {
		t.Fatal("missing list age", string(out), err)
	}
	for _, detached := range []bool{false, true} {
		name := fmt.Sprintf("target-%t", detached)
		b, pid := e.attach(name)
		targetProcess, err := readSetupTask(pid)
		if err != nil || targetProcess.parent != keepProcess.parent {
			t.Fatalf("shells do not share a daemon: %+v %+v %v", keepProcess, targetProcess, err)
		}
		// A child that ignores TERM/HUP exercises the bounded escalation path.
		childFile := filepath.Join(e.dir, name+".child")
		shellOutput(t, b, "sh -c 'trap \"\" TERM HUP; echo $$ > "+shellQuote(childFile)+"; exec sleep 120' & printf 'CHILD_%s\\n' ready", "CHILD_ready")
		var child int
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(childFile)
			if _, err := fmt.Sscan(string(data), &child); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if child == 0 {
			t.Fatal("child did not start")
		}
		age := query(name).AgeSeconds
		if age == nil {
			t.Fatal("missing age")
		}
		if detached {
			sendTest(t, b.in, message{Type: "detach"})
			untilType(t, b.out, "detached")
			b.close()
			b, resumedPID := e.attach(name)
			if resumedPID != pid || query(name).AgeSeconds == nil || *query(name).AgeSeconds < *age {
				t.Fatal("age or identity reset")
			}
			sendTest(t, b.in, message{Type: "detach"})
			untilType(t, b.out, "detached")
			b.close()
		}
		if out, err := run(name, "999.0.0"); err == nil {
			t.Fatal("accepted mismatch", out)
		}
		if query(name).PID != pid {
			t.Fatal("mismatch ended shell")
		}
		if out, err := run(name, version); err != nil || !strings.Contains(out, "Ended shell "+name) {
			t.Fatal(out, err)
		}
		for _, taskPID := range []int{pid, child} {
			if task, err := readSetupTask(taskPID); err == nil && task.state != "Z" {
				t.Fatalf("task %d survived", taskPID)
			}
		}
		if out, err := run(name, version); err == nil {
			t.Fatal("missing target accepted", out)
		}
		shellOutput(t, keep, "printf 'STILL_%s\\n' alive", "STILL_alive")
	}
	if out, err := run("keep", version); err != nil {
		t.Fatal(out, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if process, err := readSetupTask(keepProcess.parent); err != nil || process.state == "Z" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("shared remote daemon survived its last shell")

}

func TestSessionProcessTree(t *testing.T) {
	root := t.TempDir()
	for pid, stat := range map[string]string{
		"10": "10 (bash) S 1 10 10 1 20",
		"20": "20 (codex) S 10 20 10 1 20",
		"21": "21 (worker (job)) S 20 20 10 1 20",
		"30": "30 (sleep) S 10 30 10 1 20",
		"40": "40 (unrelated) S 1 40 40 1 40",
		"50": "50 (bad\x1bname) S 30 30 10 1 20",
		"60": "invalid",
	} {
		path := filepath.Join(root, pid)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got := sessionProcessTree(root, 10)
	expected := "bash (10)\n├─ codex (20) [fg]\n│  └─ worker (job) (21) [fg]\n└─ sleep (30)\n   └─ bad�name (50)\n"
	if got != expected {
		t.Fatalf("tree:\n%s\nwant:\n%s", got, expected)
	}
	if got := sessionProcessTree(root, 99); !strings.Contains(got, "99 [process unavailable]") {
		t.Fatal(got)
	}
}

func TestForegroundSnapshot(t *testing.T) {
	root := t.TempDir()
	for pid, stat := range map[string]string{
		"20": "20 (grep) S 1 42 1",
		"3":  "3 (cat (worker)) S 1 42 1",
		"4":  "4 (background) S 1 99 1",
		"5":  "malformed",
		"6":  "6 (bad\nname) S 1 42 1",
	} {
		dir := filepath.Join(root, pid)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := foregroundPrograms(root, 42); got != "cat (worker) | bad�name | grep" {
		t.Fatal(got)
	}
	for _, group := range []int{0, 100} {
		if got := foregroundPrograms(root, group); got != "—" {
			t.Fatal(got)
		}
	}
	if got := foregroundPrograms(filepath.Join(root, "missing"), 42); got != "—" {
		t.Fatal(got)
	}
}

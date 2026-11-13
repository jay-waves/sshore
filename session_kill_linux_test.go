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
	keep, _ := e.attach("keep")
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
			t.Fatal("mismatch ended session")
		}
		if out, err := run(name, version); err != nil || !strings.Contains(out, "Ended session "+name) {
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
}

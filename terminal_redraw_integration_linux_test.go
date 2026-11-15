package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func approvedTestAttach(t *testing.T, e *testEnv, name string, pid int) *bridgeConn {
	t.Helper()
	b := e.bridge(name)
	sendTest(t, b.in, message{Type: "attach", Action: "approved", PID: pid, Term: "xterm", Cols: 80, Rows: 24})
	if m := recv(t, b.out); m.Type != "ready" {
		t.Fatal(m)
	}
	var err error
	b.tty, err = b.mux.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if m := recv(t, b.tty); m.Type != "terminal" {
		t.Fatal(m)
	}
	return b
}

func resumeTestOutput(t *testing.T, b *bridgeConn) {
	t.Helper()
	sendTest(t, b.in, message{Type: "resume_output"})
	if m := recv(t, b.out); m.Type != "output_resumed" {
		t.Fatal(m)
	}
}

func expectTerminalText(t *testing.T, b *bridgeConn, marker string) string {
	t.Helper()
	var output strings.Builder
	_ = b.tty.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer b.tty.SetReadDeadline(time.Time{})
	for i := 0; i < 100; i++ {
		m, err := readMessage(b.tty)
		if err != nil {
			t.Fatalf("waiting for %q: %v; output=%q", marker, err, output.String())
		}
		if m.Type != "output" {
			t.Fatalf("expected %q, got %+v", marker, m)
		}
		output.Write(m.Data)
		if strings.Contains(output.String(), marker) {
			return output.String()
		}
	}
	t.Fatalf("missing %q: %q", marker, output.String())
	return ""
}

func detachTestClient(t *testing.T, b *bridgeConn) {
	t.Helper()
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	b.close()
}

func TestReconnectRestoresMouseAndRedrawsForegroundJob(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("redraw")
	// A foreground child with its own process group proves we do not merely
	// signal the persistent shell. Split markers avoid matching input echo.
	shellOutput(t, b, `sh -c 'trap '\''printf "REDRAW-%s\n" ready'\'' WINCH; printf "\033[?1003;1006hJOB-%s\n" ready; while :; do sleep 1; done'`, "JOB-ready")
	detachTestClient(t, b)
	b, _ = e.attach("redraw")
	assertMouseReplay(t, message{Type: "output", Data: []byte(expectTerminalText(t, b, "REDRAW-ready"))}, true)
	detachTestClient(t, b)
	b = approvedTestAttach(t, e, "redraw", pid)
	resumeTestOutput(t, b)
	assertMouseReplay(t, message{Type: "output", Data: []byte(expectTerminalText(t, b, "REDRAW-ready"))}, true)
	sendTest(t, b.tty, message{Type: "input", Data: []byte{3}})
	shellOutput(t, b, "printf 'SHELL-%s' ready", "SHELL-ready")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
}

func assertMouseReplay(t *testing.T, m message, enabled bool) {
	t.Helper()
	if m.Type != "output" || !strings.Contains(string(m.Data), "\x1b[?1003l") {
		t.Fatalf("missing mouse reset: %+v", m)
	}
	for _, mode := range []string{"1003", "1006"} {
		if strings.Contains(string(m.Data), "\x1b[?"+mode+"h") != enabled {
			t.Fatalf("mouse %s state: %q", mode, m.Data)
		}
	}
}

func TestReconnectRedrawsBashWithoutExecutingInput(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	e := newEnv(t)
	home := t.TempDir()
	// System bashrc may overwrite an inherited PS1; user bashrc runs afterwards.
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("PS1='READY-PROMPT> '\nHISTFILE=/dev/null\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e.env = append(e.env, "SHELL="+bash, "HOME="+home)
	b, _ := e.attach("bashprompt")
	expectTerminalText(t, b, "READY-PROMPT> ")
	marker := filepath.Join(t.TempDir(), "executed")
	// Leave a complete command unsubmitted. Reattach must redraw it, never send Enter.
	sendTest(t, b.tty, message{Type: "input", Data: []byte("touch " + shellQuote(marker))})
	expectTerminalText(t, b, "touch ")
	detachTestClient(t, b)
	b, _ = e.attach("bashprompt")
	expectTerminalText(t, b, "READY-PROMPT> ")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("reattach executed pending input: %v", err)
	}
	sendTest(t, b.tty, message{Type: "input", Data: []byte{21}})
	shellOutput(t, b, "printf 'BASH-%s' alive", "BASH-alive")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
	b.close()
}

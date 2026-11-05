package main

import (
	"strings"
	"testing"
	"time"
)

func approvedTestAttach(t *testing.T, e *testEnv, name string, pid int) *bridgeConn {
	t.Helper()
	b := e.bridge(name)
	sendTest(t, b.in, message{Type: "attach", Version: protocolVersion, Action: "approved", PID: pid, Term: "xterm", Cols: 80, Rows: 24})
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

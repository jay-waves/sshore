package main

import (
	"strings"
	"testing"
)

func TestMouseModesRestoredOnAttachAndApproval(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("mouse")
	shellOutput(t, b, "printf '\\033[?1003;1006hMOUSE-%s' ready", "MOUSE-ready")
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	b.close()

	b, _ = e.attach("mouse")
	assertMouseReplay(t, recv(t, b.tty), true)
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	b.close()

	b = e.bridge("mouse")
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
	sendTest(t, b.in, message{Type: "resume_output"})
	if m := recv(t, b.out); m.Type != "output_resumed" {
		t.Fatal(m)
	}
	assertMouseReplay(t, recv(t, b.tty), true)

	shellOutput(t, b, "printf '\\033[?1003;1006lMOUSE-%s' stopped", "MOUSE-stopped")
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	b.close()
	b, _ = e.attach("mouse")
	assertMouseReplay(t, recv(t, b.tty), false)
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

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
	replied := map[string]int{}
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
		// Zellij asks for pixel sizes on SIGWINCH. This test endpoint has no
		// emulator, so answer the two standard queries for our 80x24 terminal.
		for query, response := range map[string]string{"\x1b[14t": "\x1b[4;384;640t", "\x1b[16t": "\x1b[6;16;8t"} {
			count := strings.Count(output.String(), query)
			for replied[query] < count {
				sendTest(t, b.tty, message{Type: "input", Data: []byte(response)})
				replied[query]++
			}
		}
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

func TestReconnectRedrawsForegroundJobAtSameSize(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("redraw")
	// A foreground child with its own process group proves we do not merely
	// signal the persistent shell. Split markers avoid matching input echo.
	shellOutput(t, b, `sh -c 'trap '\''printf "REDRAW-%s\n" ready'\'' WINCH; printf "JOB-%s\n" ready; while :; do sleep 1; done'`, "JOB-ready")
	detachTestClient(t, b)
	b, _ = e.attach("redraw")
	expectTerminalText(t, b, "REDRAW-ready")
	detachTestClient(t, b)
	b = approvedTestAttach(t, e, "redraw", pid)
	resumeTestOutput(t, b)
	expectTerminalText(t, b, "REDRAW-ready")
	sendTest(t, b.tty, message{Type: "input", Data: []byte{3}})
	shellOutput(t, b, "printf 'SHELL-%s' ready", "SHELL-ready")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
}

func TestTmuxRedrawOnReconnectAndApproval(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	e := newEnv(t)
	socket := filepath.Join(e.dir, "multiplexer")
	t.Cleanup(func() { _ = exec.Command(tmux, "-S", socket, "kill-server").Run() })
	b, pid := e.attach("tmux")
	command := "env -u TMUX " + shellQuote(tmux) + " -S " + shellQuote(socket) + " -f /dev/null new-session -s test " + shellQuote("printf 'PANE-%s\\n' ready; exec /bin/sh") + " \\; set-option -g mouse on; printf 'MUX-%s\\n' exited"

	sendTest(t, b.tty, message{Type: "input", Data: []byte(command + "\n")})
	expectTerminalText(t, b, "PANE-ready")
	detachTestClient(t, b)
	b, _ = e.attach("tmux")
	expectTerminalText(t, b, "PANE-ready")
	detachTestClient(t, b)
	b = approvedTestAttach(t, e, "tmux", pid)
	resumeTestOutput(t, b)
	expectTerminalText(t, b, "PANE-ready")
	if err := exec.Command(tmux, "-S", socket, "kill-server").Run(); err != nil {
		t.Fatal(err)
	}
	expectTerminalText(t, b, "MUX-exited")
	shellOutput(t, b, "printf 'SHELL-%s' ready", "SHELL-ready")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
}

func TestZellijMouseAndInputOnReconnectAndApproval(t *testing.T) {
	zellij, err := exec.LookPath("zellij")
	if err != nil {
		t.Skip("zellij not installed")
	}
	e := newEnv(t)
	for _, key := range []string{"XDG_RUNTIME_DIR", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME"} {
		dir := filepath.Join(e.dir, key)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		e.env = append(e.env, key+"="+dir)
	}
	config := filepath.Join(e.dir, "config.kdl")
	if err := os.WriteFile(config, []byte("show_startup_tips false\nshow_release_notes false\nsession_serialization false\nmouse_mode true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	name := "sshore-" + filepath.Base(e.dir)
	stop := func() error { cmd := exec.Command(zellij, "kill-session", name); cmd.Env = e.env; return cmd.Run() }
	t.Cleanup(func() { _ = stop() })
	b, pid := e.attach("zellij")
	command := "env -u ZELLIJ -u ZELLIJ_SESSION_NAME " + shellQuote(zellij) + " --config " + shellQuote(config) + " --session " + shellQuote(name) + "; printf 'MUX-%s\\n' exited"
	sendTest(t, b.tty, message{Type: "input", Data: []byte(command + "\n")})
	expectTerminalText(t, b, "\x1b]0;"+name)
	shellOutput(t, b, "printf 'ZELLIJ-%s\\n' ready", "ZELLIJ-ready")
	detachTestClient(t, b)
	b, _ = e.attach("zellij")
	// Zellij queries pixel dimensions on SIGWINCH but does not necessarily
	// repaint unchanged cells. Verify the notification, restored modes and input.
	output := expectTerminalText(t, b, "\x1b[16t")
	assertMouseReplay(t, message{Type: "output", Data: []byte(output)}, true)
	shellOutput(t, b, "printf 'RECONNECTED-%s\\n' ready", "RECONNECTED-ready")
	detachTestClient(t, b)
	b = approvedTestAttach(t, e, "zellij", pid)
	resumeTestOutput(t, b)
	output = expectTerminalText(t, b, "\x1b[16t")
	assertMouseReplay(t, message{Type: "output", Data: []byte(output)}, true)
	shellOutput(t, b, "printf 'APPROVED-%s\\n' ready", "APPROVED-ready")
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	expectTerminalText(t, b, "MUX-exited")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
}

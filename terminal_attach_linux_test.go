package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestClientPrefixKeysAndDetach(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	background, _ := e.attach("background")
	sendTest(t, background.in, message{Type: "detach"})
	untilType(t, background.out, "detached")
	background.close()
	cmd, terminal, wait := startPrefixClient(t, e)
	wait("Background tasks:")
	wait("launch/background")
	var err error
	wait("Connected")
	// Prefix and command arrive in separate reads.
	if _, err = terminal.Write([]byte{2}); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(terminal, "?"); err != nil {
		t.Fatal(err)
	}
	wait("send prefix")
	if _, err = terminal.Write([]byte{2, 'r', 2, 'i'}); err != nil {
		t.Fatal(err)
	}
	wait("[sshore] fixture / keys")
	marker := filepath.Join(e.dir, "before-detach")
	// The terminal-stream barrier must reach the PTY before control-stream detach.
	if _, err = io.WriteString(terminal, "touch "+shellQuote(marker)+"\n\x02d"); err != nil {
		t.Fatal(err)
	}
	wait("Detached;")
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	b, _ := e.attach("keys", "attach")
	shellOutput(t, b, "test -f "+shellQuote(marker)+" && printf 'INPUT-%s' drained", "INPUT-drained")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
}

func startPrefixClient(t *testing.T, e *testEnv) (*exec.Cmd, *os.File, func(string)) {
	t.Helper()
	wrapper := filepath.Join(e.dir, "ssh-keys")
	script := "#!/bin/sh\nfor last; do :; done\nexec /bin/sh -c \"$last\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, e.bin, "launch", "fixture", "keys", "--server", e.bin)
	cmd.Env = append(e.env, "SSHORE_SSH="+wrapper, "SSHORE_SOCKET=", "SSHORE_STATE_DIR="+filepath.Join(e.dir, "client-state"), "TERM=xterm-256color")
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminal.Close() })
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	chunks := make(chan string, 128)
	go func() {
		defer close(chunks)
		buf := make([]byte, 4096)
		for {
			n, err := terminal.Read(buf)
			if n > 0 {
				select {
				case chunks <- string(buf[:n]):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var output strings.Builder
	wait := func(marker string) {
		t.Helper()
		for !strings.Contains(output.String(), marker) {
			select {
			case chunk, ok := <-chunks:
				if !ok {
					t.Fatalf("client ended waiting for %q: %q", marker, output.String())
				}
				output.WriteString(chunk)
			case <-ctx.Done():
				t.Fatalf("waiting for %q: %q", marker, output.String())
			}
		}
		text := output.String()
		remaining := text[strings.Index(text, marker)+len(marker):]
		output.Reset()
		output.WriteString(remaining)
	}

	return cmd, terminal, wait
}

func TestClientDetachWithBlockedInput(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startPrefixClient(t, e)
	wait("Connected")
	if _, err := io.WriteString(terminal, "stty -echo -icanon; printf 'BLOCKED-%s' ready; sleep 30\n"); err != nil {
		t.Fatal(err)
	}
	wait("BLOCKED-ready")
	if _, err := io.WriteString(terminal, strings.Repeat("a", 32768)+"\x02d"); err != nil {
		t.Fatal(err)
	}
	wait("Input delivery unconfirmed; detaching.")
	wait("Detached;")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	b, _ := e.attach("keys", "attach")
	// Reattachment succeeds while the foreground program still refuses input.
	// Close this test attachment to terminate the deliberately blocked fixture.
	b.close()
}

func TestClientSessionShortcuts(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	alpha, _ := e.attach("alpha")
	detachTestClient(t, alpha)
	busy, _ := e.attach("zulu")
	cmd, terminal, wait := startPrefixClient(t, e)
	wait("Connected fixture / keys")
	press := func(key byte) {
		t.Helper()
		if _, err := terminal.Write([]byte{2, key}); err != nil {
			t.Fatal(err)
		}
	}
	press('n')
	wait("Connected fixture / alpha")
	// Stable lexical order wraps, and an attached zulu is never taken over.
	press('p')
	wait("Connected fixture / keys")
	press('l')
	wait("Connected fixture / alpha")
	press('s')
	wait("Session # (Esc cancels):")
	// The list contains alpha and keys; select keys (second).
	if _, err := io.WriteString(terminal, "2\r"); err != nil {
		t.Fatal(err)
	}
	wait("Connected fixture / keys")
	press('c')
	wait("Connected fixture / pikachu")
	press('d')
	wait("Detached;")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// Explicitly end the independent detached sessions created by the test.
	for _, name := range []string{"alpha", "keys", "pikachu"} {
		b, _ := e.attach(name, "attach")
		sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
		untilType(t, b.tty, "exit")
	}
	sendTest(t, busy.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, busy.tty, "exit")
}

func TestClientFailedSwitchRestoresOriginal(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	alpha, _ := e.attach("alpha")
	detachTestClient(t, alpha)
	cmd, terminal, wait := startPrefixClient(t, e)
	wait("Connected fixture / keys")
	// Fail exactly the next SSH launch, after preflight has accepted alpha.
	flag := filepath.Join(e.dir, "switch-failed")
	script := "#!/bin/sh\nif [ ! -e " + shellQuote(flag) + " ]; then : > " + shellQuote(flag) + "; exit 42; fi\nfor last; do :; done\nexec /bin/sh -c \"$last\"\n"
	if err := os.WriteFile(filepath.Join(e.dir, "ssh-keys"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write([]byte{2, 'n'}); err != nil {
		t.Fatal(err)
	}
	wait("Target unavailable; restoring keys.")
	wait("Connected fixture / keys")
	if _, err := io.WriteString(terminal, "printf 'RESTORED-%s' input\n"); err != nil {
		t.Fatal(err)
	}
	wait("RESTORED-input")
	if _, err := terminal.Write([]byte{2, 'd'}); err != nil {
		t.Fatal(err)
	}
	wait("Detached;")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "keys"} {
		b, _ := e.attach(name, "attach")
		sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
		untilType(t, b.tty, "exit")
	}
}

func approvedTestAttach(t *testing.T, e *testEnv, name string, pid int, tree ...shellTreeNode) *bridgeConn {
	t.Helper()
	b := e.bridge(name)
	sendTest(t, b.in, message{Type: "attach", Tree: tree, Action: "approved", PID: pid, Term: "xterm", Cols: 80, Rows: 24})
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
	answeredDA, answeredCPR := 0, 0
	_ = b.tty.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer b.tty.SetReadDeadline(time.Time{})
	for i := 0; i < 100; i++ {
		m, err := readMessage(b.tty)
		if err != nil {
			t.Fatalf("waiting for %q: %v; output=%q", marker, err, output.String())
		}
		if m.Type == "exit" && marker == "" {
			return output.String()
		}
		if m.Type != "output" {
			t.Fatalf("expected %q, got %+v", marker, m)
		}
		output.Write(m.Data)
		// Emulate the basic xterm replies fish uses as input barriers.
		all := output.String()
		for n := strings.Count(all, "\x1b[0c"); answeredDA < n; answeredDA++ {
			sendTest(t, b.tty, message{Type: "input", Data: []byte("\x1b[?1;2c")})
		}
		for n := strings.Count(all, "\x1b[6n"); answeredCPR < n; answeredCPR++ {
			sendTest(t, b.tty, message{Type: "input", Data: []byte("\x1b[1;1R")})
		}
		if marker != "" && strings.Contains(output.String(), marker) {
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
	shellOutput(t, b, `sh -c 'trap '\''printf "REDRAW-%s\n" ready'\'' WINCH; printf "\033[?1003;1006;1004;2004hJOB-%s\n" ready; while :; do sleep 1; done'`, "JOB-ready")
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
	expectTerminalText(t, b, "")
}

func assertMouseReplay(t *testing.T, m message, enabled bool) {
	t.Helper()
	if m.Type != "output" || !strings.Contains(string(m.Data), "\x1b[?1003l") {
		t.Fatalf("missing mouse reset: %+v", m)
	}
	for _, mode := range []string{"1003", "1006", "1004", "2004"} {
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
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("printf x >> ~/.startup-count\n. ~/.bashrc\n"), 0600); err != nil {
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
	if data, err := os.ReadFile(filepath.Join(home, ".startup-count")); err != nil || string(data) != "x" {
		t.Fatalf("startup rerun: %q %v", data, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("reattach executed pending input: %v", err)
	}
	sendTest(t, b.tty, message{Type: "input", Data: []byte{21}})
	// Keep the cursor inside the argument across reattachment. Inserting at
	// the restored position proves buffer and cursor preservation together.
	sendTest(t, b.tty, message{Type: "input", Data: []byte("printf 'EDIT-%s\\n' ac\x1b[D")})
	expectTerminalText(t, b, "printf ")
	detachTestClient(t, b)
	b, _ = e.attach("bashprompt")
	expectTerminalText(t, b, "READY-PROMPT> ")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("b\n")})
	expectTerminalText(t, b, "EDIT-abc")
	shellOutput(t, b, "printf 'BASH-%s' alive", "BASH-alive")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	expectTerminalText(t, b, "")
	b.close()
}

func TestReconnectPreservesStoppedJob(t *testing.T) {
	for _, name := range []string{"bash", "zsh", "fish"} {
		t.Run(name, func(t *testing.T) {
			path, err := exec.LookPath(name)
			if err != nil {
				t.Skip(name + " is unavailable")
			}
			stopped, running := "Stopped", "Running"
			if name == "zsh" {
				stopped, running = "suspended", "running"
			}
			if name == "fish" {
				stopped, running = "stopped", "running"
			}
			e := newEnv(t)
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte("PROMPT='JOB-PROMPT> '\n"), 0600); err != nil {
				t.Fatal(err)
			}
			e.env = append(e.env, "SHELL="+path, "HOME="+home, "ZDOTDIR="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
			b, _ := e.attach("jobcontrol")
			pidFile := filepath.Join(home, "job-pid")
			responsiveShellOutput(t, b, "sh -c "+shellQuote("echo $$ > "+shellQuote(pidFile)+"; printf 'JOB-%s\\n' started; exec sleep 120"), "JOB-started")
			sendTest(t, b.tty, message{Type: "input", Data: []byte{26}})
			expectTerminalText(t, b, stopped)
			if name == "fish" {
				expectTerminalText(t, b, "\x1b]133;B")
			}
			detachTestClient(t, b)
			b, _ = e.attach("jobcontrol")
			responsiveShellOutput(t, b, "jobs -l", stopped)
			// A stopped job must remain resumable in the background and foreground.
			responsiveShellOutput(t, b, "bg; jobs", running)
			sendTest(t, b.tty, message{Type: "input", Data: []byte("fg\n")})
			expectTerminalText(t, b, "fg")
			jobPID, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				data, err := os.ReadFile("/proc/" + strings.TrimSpace(string(jobPID)) + "/stat")
				if err != nil {
					t.Fatal(err)
				}
				fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+1:])
				if len(fields) > 5 && fields[2] == fields[5] {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("job did not regain foreground: %s", data)
				}
				time.Sleep(5 * time.Millisecond)
			}
			sendTest(t, b.tty, message{Type: "input", Data: []byte{3}})
			if name == "fish" {
				expectTerminalText(t, b, "\x1b]133;B")
			}
			responsiveShellOutput(t, b, "printf 'CONTROL-%s' restored", "CONTROL-restored")
			sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
			expectTerminalText(t, b, "")
			b.close()
		})
	}
}

func TestReconnectPreservesShellEditor(t *testing.T) {
	for _, name := range []string{"zsh", "fish"} {
		t.Run(name, func(t *testing.T) {
			path, err := exec.LookPath(name)
			if err != nil {
				t.Skip(name + " is unavailable")
			}
			e := newEnv(t)
			home := t.TempDir()
			config := filepath.Join(home, ".zshrc")
			content := "PROMPT='EDITOR-READY> '\nRPROMPT=''\nbindkey -e\n"
			if name == "fish" {
				config = filepath.Join(home, ".config", "fish", "config.fish")
				content = "set -g fish_greeting\nfunction fish_prompt; printf 'EDITOR-READY> '; end\nfunction fish_right_prompt; end\n"
			}
			if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			e.env = append(e.env, "SHELL="+path, "HOME="+home, "ZDOTDIR="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
			b, _ := e.attach("editor")
			expectTerminalText(t, b, "EDITOR-READY> ")
			marker := filepath.Join(home, "submitted")
			sendTest(t, b.tty, message{Type: "input", Data: []byte("printf '%s' ac > " + shellQuote(marker) + "; printf 'DONE-%s\\n' ready")})
			expectTerminalText(t, b, "printf")
			// Move to the beginning and then into the two-character argument.
			sendTest(t, b.tty, message{Type: "input", Data: []byte("\x01" + strings.Repeat("\x1b[C", len("printf '%s' a")))})
			detachTestClient(t, b)
			b, _ = e.attach("editor")
			expectTerminalText(t, b, "EDITOR-READY> ")
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("pending command executed: %v", err)
			}
			sendTest(t, b.tty, message{Type: "input", Data: []byte("b\n")})
			expectTerminalText(t, b, "DONE-ready")
			if data, err := os.ReadFile(marker); err != nil || string(data) != "abc" {
				t.Fatalf("editor state lost: %q, %v", data, err)
			}
			sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
			expectTerminalText(t, b, "")
			b.close()
		})
	}
}

func responsiveShellOutput(t *testing.T, b *bridgeConn, command, marker string) string {
	t.Helper()
	sendTest(t, b.tty, message{Type: "input", Data: []byte(command + "\n")})
	return expectTerminalText(t, b, marker)
}

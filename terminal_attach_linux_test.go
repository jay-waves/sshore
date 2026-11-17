package main

import (
	"context"
	"encoding/json"
	"github.com/jay-waves/sshore/internal/sshtest"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
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
	wait("(background)")
	var err error
	wait("Connected")
	// Prefix and command arrive in separate reads.
	if _, err = terminal.Write([]byte{2}); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(terminal, "r"); err != nil {
		t.Fatal(err)
	}
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
	return startSessionClient(t, e, []string{"launch", "fixture", "keys", "--server", e.bin})
}

func startSessionClient(t *testing.T, e *testEnv, args []string) (*exec.Cmd, *os.File, func(string)) {
	t.Helper()
	wrapper := filepath.Join(e.dir, "ssh-keys")
	script := "#!/bin/sh\nunset SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN SSHORE_LOCAL_SESSION SSHORE_SOCKET\nfor last; do :; done\nexec /bin/sh -c \"$last\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	wrapper = sshtest.Wrap(t, wrapper, e.env)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, e.bin, args...)
	cmd.Env = append(e.env, "SSHORE_SSH="+wrapper, "SSHORE_SOCKET=", "SSHORE_STATE_DIR="+filepath.Join(e.dir, "client-state"), "TERM=xterm-256color", "SSHORE_TEST_CONFIG_CONTEXT="+filepath.Join(e.dir, "config-context"))
	t.Cleanup(func() { shutdownFixtureDaemon(t, filepath.Join(e.dir, "client-state")) })
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

func TestClientLocalRemoteHandoff(t *testing.T) {
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startSessionClient(t, e, []string{"launch", "--local", "origin", "--no-profile"})
	t.Cleanup(func() {
		cleanup := exec.Command(e.bin, "kill", "--all")
		cleanup.Env = cmd.Env
		_ = cleanup.Run()
	})
	wait("Connected local / origin")
	wait("sshore-test> ")
	marker := filepath.Join(e.dir, "launcher-exited")
	directory := filepath.Join(e.dir, "handoff-cwd")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nexec " + shellQuote(localEnvironmentValue(cmd.Env, "SSHORE_SSH")) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(directory, "ssh"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	// The command returns to its owner shell while the frontend is remote.
	command := "cd " + shellQuote(directory) + "; export HANDOFF_VALUE=preserved; export PATH=" + shellQuote(directory) + ":$PATH; unset SSHORE_SSH; " + shellQuote(e.bin) + " spawn fixture remote --quiet --server " + shellQuote(e.bin) + "; printf '%s' \"$?\" > " + shellQuote(marker) + "\n"
	io.WriteString(terminal, command)
	wait("Connected fixture / remote")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			if string(data) != "0" {
				t.Fatalf("launcher failed: %s", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nested launcher remained running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Redraw remains local to the frontend; ordinary input reaches the remote shell.
	io.WriteString(terminal, "\x02rprintf 'HANDOFF_%s\\n' remote\n")
	wait("HANDOFF_remote")
	contextData, err := os.ReadFile(filepath.Join(e.dir, "config-context"))
	if err != nil || string(contextData) != directory+"\npreserved\n" {
		t.Fatalf("SSH config context: %q %v", contextData, err)
	}
	rename := exec.Command(e.bin, "rename", "--local", "origin", "returned")
	rename.Env = cmd.Env
	if out, err := rename.CombinedOutput(); err != nil {
		t.Fatalf("rename return target: %s %v", out, err)
	}
	io.WriteString(terminal, "\x02d")
	wait("Connected local / returned")
	io.WriteString(terminal, "printf 'HANDOFF_%s\\n' returned\n")
	wait("HANDOFF_returned")
	io.WriteString(terminal, shellQuote(e.bin)+" attach fixture remote --quiet --server "+shellQuote(e.bin)+"\n")
	wait("Connected fixture / remote")
	io.WriteString(terminal, "exit\n")
	wait("Connected local / returned")
	// Failed connection restores the same local shell, rather than exiting it.
	io.WriteString(terminal, shellQuote(e.bin)+" attach fixture missing --server "+shellQuote(e.bin)+"\n")
	wait("Target unavailable; restoring returned")
	wait("Connected local / returned")
	// The failed launcher now reports failure to its parent shell.
	io.WriteString(terminal, "exit 0\n")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestClientDetachWithBlockedInput(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startPrefixClient(t, e)
	wait("Connected")
	if _, err := io.WriteString(terminal, "stty -echo -icanon; printf 'BLOCKED-%s' ready; read interrupted_input\n"); err != nil {
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
	wait("Connected fixture /")
	press('d')
	wait("Detached;")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// Explicitly end the independent detached sessions created by the test.
	inventoryCmd := exec.Command(e.bin, "inventory", version)
	inventoryCmd.Env = e.env
	data, err := inventoryCmd.Output()
	var inv inventory
	if err != nil || json.Unmarshal(data, &inv) != nil {
		t.Fatalf("inventory: %s %v", data, err)
	}
	for _, shell := range inv.Shells {
		name := shell.Name
		if name == "zulu" {
			continue
		}
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

func reattachTestWithTree(t *testing.T, e *testEnv, name string, pid int, tree ...shellTreeNode) *bridgeConn {
	t.Helper()
	id, err := resolveSession(name)
	if err != nil {
		t.Fatal(err)
	}
	b := e.bridge(name)
	sendTest(t, b.in, message{Type: "attach", Action: "existing", Instance: id, Tree: tree, Term: "xterm", Cols: 80, Rows: 24})
	if m := recv(t, b.out); m.Type != "ready" || m.PID != pid {
		t.Fatalf("reattach: %+v", m)
	}
	b.tty, err = b.mux.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if m := recv(t, b.tty); m.Type != "terminal" {
		t.Fatal(m)
	}
	return b
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
	// Match the client: drain terminal input before detaching on control.
	sendTest(t, b.tty, message{Type: "input_barrier"})
	untilType(t, b.tty, "input_drained")
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
	var resumedPID int
	b, resumedPID = e.attach("redraw", "attach")
	if resumedPID != pid {
		t.Fatal("reattachment replaced the shell")
	}
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
			home := configureReconnectShell(t, e, path, "JOB-PROMPT> ")
			b, _ := e.attach("jobcontrol")
			expectTerminalText(t, b, "JOB-PROMPT> ")
			pidFile := filepath.Join(home, "job-pid")
			responsiveShellOutput(t, b, "sh -c "+shellQuote("echo $$ > "+shellQuote(pidFile)+"; printf 'JOB-%s\\n' started; exec sleep 120"), "JOB-started")
			sendTest(t, b.tty, message{Type: "input", Data: []byte{26}})
			marker := stopped
			if name == "fish" {
				marker = "JOB-PROMPT> "
			}
			if output := expectTerminalText(t, b, marker); !strings.Contains(output, stopped) {
				t.Fatalf("job did not stop: %q", output)
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
				expectTerminalText(t, b, "JOB-PROMPT> ")
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
			home := configureReconnectShell(t, e, path, "EDITOR-READY> ")
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

// Use fixture-owned prompts and startup configuration for reconnect assertions.
func configureReconnectShell(t *testing.T, e *testEnv, path, prompt string) string {
	t.Helper()
	home := t.TempDir()
	files := map[string]string{}
	switch filepath.Base(path) {
	case "bash":
		files[".bash_profile"] = ". ~/.bashrc\n"
		files[".bashrc"] = "PS1=" + shellQuote(prompt) + "\nHISTFILE=/dev/null\n"
	case "zsh":
		// Keep the fixture's .zshrc while skipping system completion setup.
		files[".zshenv"] = "unsetopt GLOBAL_RCS\n"
		files[".zshrc"] = "PROMPT=" + shellQuote(prompt) + "\nRPROMPT=''\nbindkey -e\n"
	case "fish":
		files[filepath.Join(".config", "fish", "config.fish")] = "set -g fish_greeting\nfunction fish_prompt; printf '%s' " + shellQuote(prompt) + "; end\nfunction fish_right_prompt; end\n"
	}
	for name, content := range files {
		config := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e.env = append(e.env, "SHELL="+path, "HOME="+home, "ZDOTDIR="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	return home
}

func responsiveShellOutput(t *testing.T, b *bridgeConn, command, marker string) string {
	t.Helper()
	sendTest(t, b.tty, message{Type: "input", Data: []byte(command + "\n")})
	return expectTerminalText(t, b, marker)
}

func TestClientNestedLocalSpawnHandoff(t *testing.T) {
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startSessionClient(t, e, []string{"launch", "--local", "root", "--no-profile"})
	t.Cleanup(func() { cleanup := exec.Command(e.bin, "kill", "--all"); cleanup.Env = cmd.Env; _ = cleanup.Run() })
	wait("Connected local / root")
	wait("sshore-test> ")
	for _, name := range []string{"child", "grandchild"} {
		marker := filepath.Join(e.dir, name+"-returned")
		io.WriteString(terminal, shellQuote(e.bin)+" spawn --local "+name+" --no-profile; printf done > "+shellQuote(marker)+"\n")
		wait("Connected local / " + name)
		wait("sshore-test> ")
		deadline := time.Now().Add(3 * time.Second)
		for {
			data, err := os.ReadFile(marker)
			if err == nil && string(data) == "done" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("nested launcher did not exit", name, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for _, name := range []string{"child", "root"} {
		io.WriteString(terminal, "exit\n")
		wait("Connected local / " + name)
	}
	io.WriteString(terminal, "exit\n")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestClientNestedRemoteSpawnReusesAuthentication(t *testing.T) {
	for _, command := range []string{"spawn", "launch"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			t.Setenv("SSHORE_SOCKET", "")
			e := newEnv(t)
			cmd, terminal, wait := startSessionClient(t, e, []string{command, "fixture", "root", "--no-login", "--no-profile", "--server", e.bin})
			wait("Connected fixture / root")
			io.WriteString(terminal, "printf 'READY_%s\\n' shell\n")
			wait("READY_shell")
			// Any new authentication attempt now fails at SSH configuration lookup.
			ssh := localEnvironmentValue(cmd.Env, "SSHORE_SSH")
			if err := os.WriteFile(ssh, []byte("#!/bin/sh\necho unexpected-reauthentication >&2\nexit 91\n"), 0700); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				io.WriteString(terminal, shellQuote(e.bin)+" spawn --no-login --no-profile\n")
				wait("Connected fixture / ")
				io.WriteString(terminal, "printf 'READY_%s\\n' shell\n")
				wait("READY_shell")
			}
			for range 2 {
				io.WriteString(terminal, "exit\n")
				wait("Connected fixture / ")
				io.WriteString(terminal, "printf 'READY_%s\\n' shell\n")
				wait("READY_shell")
			}
			io.WriteString(terminal, "exit\n")
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClientSpawnShortcutReturnsToParent(t *testing.T) {
	for _, tc := range []struct{ command, host string }{{"launch", "--local"}, {"spawn", "--local"}, {"launch", "fixture"}, {"spawn", "fixture"}} {
		host := tc.host
		t.Run(tc.command+"/"+host, func(t *testing.T) {
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			t.Setenv("SSHORE_SOCKET", "")
			e := newEnv(t)
			args := []string{tc.command, host, "root", "--no-login", "--no-profile"}
			label := "local"
			if host != "--local" {
				args = append(args, "--server", e.bin)
				label = host
			}
			cmd, terminal, wait := startSessionClient(t, e, args)
			t.Cleanup(func() { cleanup := exec.Command(e.bin, "kill", "--all"); cleanup.Env = cmd.Env; _ = cleanup.Run() })
			wait("Connected " + label + " / root")
			ready := func() {
				io.WriteString(terminal, "printf 'SHORTCUT_%s\\n' ready\n")
				wait("SHORTCUT_ready")
			}
			ready()
			for range 2 {
				io.WriteString(terminal, "\x02c")
				wait("(spawn)")
				ready()
			}
			for range 2 {
				io.WriteString(terminal, "exit\n")
				wait("Connected " + label + " / ")
				ready()
			}
			io.WriteString(terminal, "exit\n")
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Exercise both terminal bytes and an actual OS interrupt: console hosts can
// deliver either, but neither should disconnect an established attachment.
func TestClientInterruptStaysAttached(t *testing.T) {
	for _, host := range []string{"--local", "fixture"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			t.Setenv("SSHORE_SOCKET", "")
			e := newEnv(t)
			args := []string{"launch", host, "interrupt", "--no-login", "--no-profile"}
			label := "local"
			if host != "--local" {
				label = host
				args = append(args, "--server", e.bin)
			}
			cmd, terminal, wait := startSessionClient(t, e, args)
			t.Cleanup(func() { cleanup := exec.Command(e.bin, "kill", "--all"); cleanup.Env = cmd.Env; _ = cleanup.Run() })
			wait("Connected " + label + " / interrupt")
			for _, systemSignal := range []bool{false, true} {
				io.WriteString(terminal, "trap 'printf \"CAUGHT_%s\\n\" interrupt' INT; printf 'WAIT_%s\\n' ready; read interrupted_input\n")
				wait("WAIT_ready")
				if systemSignal {
					if err := cmd.Process.Signal(os.Interrupt); err != nil {
						t.Fatal(err)
					}
				} else {
					terminal.Write([]byte{3})
				}
				wait("CAUGHT_interrupt")
				io.WriteString(terminal, "printf 'STILL_%s\\n' attached\n")
				wait("STILL_attached")
			}
			io.WriteString(terminal, "exit\n")
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClientInterruptDuringSwitchRestoresOriginal(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	alpha, _ := e.attach("alpha")
	detachTestClient(t, alpha)
	cmd, terminal, wait := startPrefixClient(t, e)
	wait("Connected fixture / keys")
	flag := filepath.Join(e.dir, "switch-started")
	release := filepath.Join(e.dir, "release-setup")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	script := "#!/bin/sh\nif [ ! -e " + shellQuote(flag) + " ]; then : > " + shellQuote(flag) + "; while [ ! -e " + shellQuote(release) + " ]; do sleep 0.01; done; fi\nfor last; do :; done\nexec /bin/sh -c \"$last\"\n"
	if err := os.WriteFile(filepath.Join(e.dir, "ssh-keys"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	terminal.Write([]byte{2, 'n'})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(flag); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("switch never reached setup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	terminal.Write([]byte{3})
	wait("Target unavailable; restoring keys.")
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	wait("Connected fixture / keys")
	io.WriteString(terminal, "printf 'RESTORED_%s\\n' input\n")
	wait("RESTORED_input")
	terminal.Write([]byte{2, 'd'})
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

// Abrupt daemon death cannot deliver an exit frame or TUI cleanup. The client
// must restore its own terminal on EOF for both persistent and spawned shells.
func TestClientDaemonLossRestoresTerminal(t *testing.T) {
	for _, command := range []string{"launch", "spawn"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("SSHORE_SOCKET", "")
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			e := newEnv(t)
			cmd, terminal, wait := startSessionClient(t, e, []string{command, "fixture", "crash", "--no-login", "--no-profile", "--server", e.bin})
			wait("Connected fixture / crash")
			// Enable exactly the event reporting that produces ;1:3u on key release.
			// Split the marker so the echoed command cannot satisfy the output barrier.
			if _, err := io.WriteString(terminal, "printf '\\033[>11u\\033[>4;2mREADY_%s' crash\n"); err != nil {
				t.Fatal(err)
			}
			wait("READY_crash")
			mode, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if mode.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG) != 0 {
				t.Fatal("client did not enter raw mode")
			}
			// Authenticate against this isolated fixture's registered daemon before
			// signaling it; never discover or signal the user's account by process name.
			record, err := existingDaemonRecord()
			if err != nil {
				t.Fatal(err)
			}
			identity, err := processStart(record.ManagerPID)
			if err != nil {
				t.Fatal(err)
			}
			c, err := dialLocalSession(record)
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
			if current, err := processStart(record.ManagerPID); err != nil || current != identity {
				t.Fatalf("test daemon identity changed: %v", err)
			}
			if err := syscall.Kill(record.ManagerPID, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			wait("\x1b[=0u\x1b[>4;0m")
			if err := cmd.Wait(); err == nil {
				t.Fatal("daemon loss reported success")
			}
			mode, err = unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			want := uint32(unix.ICANON | unix.ECHO | unix.ISIG)
			if mode.Lflag&want != want {
				t.Fatalf("terminal remained raw after daemon loss: %#x", mode.Lflag)
			}
		})
	}
}

func TestClientDetachPromotesSharedMRU(t *testing.T) {
	for _, host := range []string{"--local", "fixture"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("SSHORE_SOCKET", "")
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			e := newEnv(t)
			args := []string{"launch", host, "mru-detach", "--no-login", "--no-profile"}
			label := "local"
			if host != "--local" {
				args = append(args, "--server", e.bin)
				label = host
			}
			cmd, terminal, wait := startSessionClient(t, e, args)
			wait("Connected " + label + " / mru-detach")
			if _, err := io.WriteString(terminal, "\x02d"); err != nil {
				t.Fatal(err)
			}
			wait("Detached;")
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(e.dir, "client-state", "connections", "detached.json"))
			if err != nil {
				t.Fatal(err)
			}
			var rows []recentAttachment
			if err := json.Unmarshal(data, &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Name != "mru-detach" || rows[0].Local != (host == "--local") || rows[0].Identity == "" {
				t.Fatalf("detach MRU: %+v", rows)
			}
			// No operands must reattach the same local or remote shell using MRU.
			selectorsList := [][]string{nil, {taskUID(rows[0].Identity)[:2]}}
			if host != "--local" {
				selectorsList = append(selectorsList, []string{host})
			}
			for _, selectors := range selectorsList {
				reattach := append([]string{"attach"}, selectors...)
				next, tty, await := startSessionClient(t, e, reattach)
				await("Connected " + label + " / mru-detach")
				if _, err := io.WriteString(tty, "\x02d"); err != nil {
					t.Fatal(err)
				}
				await("Detached;")
				if err := next.Wait(); err != nil {
					t.Fatal(err)
				}
			}

		})
	}
}

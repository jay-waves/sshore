package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/jay-waves/sshore/internal/proc"
	"github.com/jay-waves/sshore/tests/testutil/sshtest"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

func TestClientPrefixKeysAndDetach(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	background, _, _ := startSessionClient(t, e, []string{"fixture-launch", "fixture", "background", "--server", e.bin, "--detach"})
	if err := background.Wait(); err != nil {
		t.Fatal(err)
	}
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
	wait("Detached from ")
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	b, _ := e.attach("keys", "attach")
	shellOutput(t, b, "test -f "+shellQuote(marker)+" && printf 'INPUT-%s' drained", "INPUT-drained")
	b.writeInput(t, []byte("exit\n"))
	b.readExit(t)
}

func startPrefixClient(t *testing.T, e *testEnv) (*exec.Cmd, *os.File, func(string)) {
	t.Helper()
	return startSessionClient(t, e, []string{"fixture-launch", "fixture", "keys", "--server", e.bin})
}

func sessionSSH(t *testing.T, e *testEnv, prelude string) string {
	return sessionSSHWithHook(t, e, prelude, nil)
}

func sessionSSHWithHook(t *testing.T, e *testEnv, prelude string, hook func(context.Context, string, cryptossh.Channel) bool) string {
	t.Helper()
	remoteEnv := append(append([]string(nil), e.env...),
		"PATH="+filepath.Dir(e.bin)+":"+localEnvironmentValue(e.env, "PATH"),
		"SSHORE_ASKPASS=", "SSHORE_ASKPASS_ADDR=", "SSHORE_ASKPASS_TOKEN=", "SSH_ASKPASS=", "SSHORE_LOCAL_SESSION=", "SSHORE_SOCKET=",
	)
	return sshtest.Start(t, sshtest.Options{AllowForward: true, ConfigPrelude: prelude, Exec: func(ctx context.Context, command string, stream cryptossh.Channel) {
		if hook != nil && hook(ctx, command, stream) {
			return
		}
		sshtest.Shell(ctx, command, stream, e.dir, remoteEnv)
	}})
}

func startSessionClient(t *testing.T, e *testEnv, args []string) (*exec.Cmd, *os.File, func(string)) {
	t.Helper()
	config := filepath.Join(e.dir, "unused-ssh")
	if !strings.HasPrefix(args[1], "here:") {
		config = sessionSSH(t, e, "")
	}
	return startSessionClientWithSSH(t, e, args, config)
}

func startSessionClientWithSSH(t *testing.T, e *testEnv, args []string, config string) (*exec.Cmd, *os.File, func(string)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, e.bin, args...)
	cmd.Env = append(e.env, "SSHORE_SSH="+config, "SSHORE_SOCKET=", "SSHORE_STATE_DIR="+filepath.Join(e.dir, "client-state"), "TERM=xterm-256color")
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
	config := sessionSSH(t, e, `printf '%s\n%s\n' "$PWD" "$HANDOFF_VALUE" > `+shellQuote(filepath.Join(e.dir, "config-context")))
	cmd, terminal, wait := startSessionClientWithSSH(t, e, []string{"fixture-launch", "here:", "origin", "--no-profile"}, config)
	t.Cleanup(func() {
		cleanup := exec.Command(e.bin, "kill", "--all")
		cleanup.Env = cmd.Env
		_ = cleanup.Run()
	})
	wait("Connected here:origin")
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
	command := "cd " + shellQuote(directory) + "; export HANDOFF_VALUE=preserved; export PATH=" + shellQuote(directory) + ":$PATH; unset SSHORE_SSH; " + shellQuote(e.bin) + " spawn fixture: --quiet --server " + shellQuote(e.bin) + "; printf '%s' \"$?\" > " + shellQuote(marker) + "\n"
	io.WriteString(terminal, command)
	wait("Connected fixture:")
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
	// Show the live handoff separately from attachment state.
	list := exec.Command(e.bin, "list", "--json")
	list.Env = cmd.Env
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("list connected shell: %s: %v", out, err)
	}
	var inv inventory
	if err := json.Unmarshal(out, &inv); err != nil {
		t.Fatalf("list JSON: %s: %v", out, err)
	}
	found := false
	for _, row := range inv.LocalShells {
		if row.Name == "origin" && row.State == "detached" && row.ConnectedHost == "fixture" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing connected return target: %s", out)
	}
	var remoteCode string
	for _, shell := range inv.Shells {
		if shell.Host == "fixture" && shell.Temporary && shell.Observed {
			remoteCode = shell.ShortID
		}
	}
	tree := exec.Command(e.bin, "pstree")
	tree.Env = cmd.Env
	out, err = tree.CombinedOutput()
	if err != nil || remoteCode == "" || !strings.Contains(string(out), "-> fixture: spawn/"+remoteCode) {
		t.Fatalf("connected tree: %s: %v", out, err)
	}
	table := exec.Command(e.bin, "list")
	table.Env = cmd.Env
	out, err = table.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "-> fixture") {
		t.Fatalf("list handoff detail: %s: %v", out, err)
	}
	// Redraw remains local to the frontend; ordinary input reaches the remote shell.
	io.WriteString(terminal, "\x02rprintf 'HANDOFF_%s\\n' remote\n")
	wait("HANDOFF_remote")
	contextData, err := os.ReadFile(filepath.Join(e.dir, "config-context"))
	if err != nil || string(contextData) != directory+"\npreserved\n" {
		t.Fatalf("SSH config context: %q %v", contextData, err)
	}
	rename := exec.Command(e.bin, "rename", "here:origin", "returned")
	rename.Env = cmd.Env
	if out, err := rename.CombinedOutput(); err != nil {
		t.Fatalf("rename return target: %s %v", out, err)
	}
	io.WriteString(terminal, "\x02d")
	wait("Connected here:returned")
	io.WriteString(terminal, "printf 'HANDOFF_%s\\n' returned\n")
	wait("HANDOFF_returned")
	io.WriteString(terminal, shellQuote(e.bin)+" attach fixture: --quiet --server "+shellQuote(e.bin)+"\n")
	wait("Connected fixture:")
	io.WriteString(terminal, "exit\n")
	wait("Connected here:returned")
	// Failed connection restores the same local shell, rather than exiting it.
	io.WriteString(terminal, shellQuote(e.bin)+" attach fixture:missing --server "+shellQuote(e.bin)+"\n")
	wait("Target unavailable; restoring returned")
	wait("Connected here:returned")
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
	// The foreground process must leave the PTY unread until detach completes.
	if _, err := io.WriteString(terminal, "stty -echo -icanon; printf 'BLOCKED-%s' ready; sleep 30\n"); err != nil {
		t.Fatal(err)
	}
	wait("BLOCKED-ready")
	if _, err := io.WriteString(terminal, strings.Repeat("a", 32768)+"\x02d"); err != nil {
		t.Fatal(err)
	}
	wait("Input delivery unconfirmed; detaching.")
	wait("Detached from ")
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
	wait("Connected fixture:keys")
	press := func(key byte) {
		t.Helper()
		if _, err := terminal.Write([]byte{2, key}); err != nil {
			t.Fatal(err)
		}
	}
	press('n')
	wait("Connected fixture:alpha")
	// Stable lexical order wraps, and an attached zulu is never taken over.
	press('p')
	wait("Connected fixture:keys")
	press('l')
	wait("Connected fixture:alpha")
	press('d')
	wait("Detached from ")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// Explicitly end the independent detached sessions created by the test.
	inventoryCmd := exec.Command(e.bin, remoteServiceArgs(remoteServiceRequest{Operation: "inventory"})...)
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
		b.writeInput(t, []byte("exit\n"))
		b.readExit(t)
	}
	busy.writeInput(t, []byte("exit\n"))
	busy.readExit(t)
}

func TestClientCommandConsole(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	alpha, _ := e.attach("alpha")
	detachTestClient(t, alpha)
	var sshCommands atomic.Int32
	config := sessionSSHWithHook(t, e, "", func(context.Context, string, cryptossh.Channel) bool {
		sshCommands.Add(1)
		return false
	})
	// Keep hosted registrations invisible to reads of the frontend's runtime.
	hostEnv := e.env
	e.env = append(slices.Clone(hostEnv), "SSHORE_RUNTIME_DIR="+filepath.Join(e.dir, "client-runtime"))
	cmd, terminal, wait := startSessionClientWithSSH(t, e, []string{"fixture-launch", "fixture", "keys", "--server", e.bin}, config)
	e.env = hostEnv
	wait("Connected fixture:keys")
	io.WriteString(terminal, "\x02:help\r")
	wait("sshore commands")
	wait("  alpha")
	wait("Switch session")
	beforeInventory := sshCommands.Load()
	io.WriteString(terminal, "ls\r")
	wait("* ")
	wait("sshore> ")
	io.WriteString(terminal, "ps\r")
	wait("alpha")
	wait("sshore> ")
	io.WriteString(terminal, "\x1b")
	wait("\x1b[?1049l")
	io.WriteString(terminal, "printf 'CONSOLE-%s' restored\n")
	wait("CONSOLE-restored")
	if got := sshCommands.Load(); got != beforeInventory {
		t.Fatalf("panel inventory started an SSH command: %d before, %d after", beforeInventory, got)
	}
	io.WriteString(terminal, "\x02:")
	wait("  alpha")
	io.WriteString(terminal, "att\tal\t\r")
	wait("Connected fixture:alpha from fixture:keys")
	io.WriteString(terminal, "\x02:")
	wait("sshore commands")
	io.WriteString(terminal, "sh\r")
	wait("(spawn)")
	io.WriteString(terminal, "\x02:ps\r")
	wait("spawn/")
	wait("  *")
	io.WriteString(terminal, "\x1b")
	wait("\x1b[?1049l")
	io.WriteString(terminal, "exit\n")
	wait("Connected fixture:alpha")
	io.WriteString(terminal, "\x02:")
	wait("sshore> ")
	beta, _ := e.attach("beta")
	detachTestClient(t, beta)
	io.WriteString(terminal, "a beta\r")
	wait("Connected fixture:beta")
	io.WriteString(terminal, "\x02:forward here:0 fixture:3000\r")
	wait("Show task inventory on this host")
	io.WriteString(terminal, "cp here:source fixture:dest\r")
	wait("Show task inventory on this host")
	io.WriteString(terminal, "exit\r")
	wait("\x1b[?1049l")
	io.WriteString(terminal, "\x02:d\r")
	wait("Detached from ")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keys", "alpha", "beta"} {
		b, _ := e.attach(name, "attach")
		b.writeInput(t, []byte("exit\n"))
		b.readExit(t)
	}
}

func TestClientLocalConsoleInventory(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startSessionClient(t, e, []string{"fixture-spawn", "here:", "panel-local", "--no-profile"})
	wait("Connected")
	io.WriteString(terminal, "\x02:list\r")
	wait("HOST  local")
	wait("* ")
	wait("panel-local")
	wait("sshore> ")
	io.WriteString(terminal, "pstree\r")
	wait("(panel-local)  *")
	wait("sshore> ")
	io.WriteString(terminal, "exit\r")
	wait("\x1b[?1049l")
	io.WriteString(terminal, "exit\n")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestRootLocalSpawnDetachConfirmation(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startSessionClient(t, e, []string{"fixture-spawn", "here:", "confirm-detach", "--no-profile"})
	wait("Connected")
	for _, cancel := range []string{"\r", "\x1b", "\x03"} {
		io.WriteString(terminal, "\x02d")
		wait("Detaching may close this terminal window")
		wait("This spawn may end when its original parent exits.")
		wait("Detach? [y/N]")
		io.WriteString(terminal, cancel)
		wait("\x1b[?1049l")
		io.WriteString(terminal, "printf 'CONFIRM_%s\\n' ALIVE\n")
		wait("CONFIRM_ALIVE")
	}
	io.WriteString(terminal, "\x02:detach\r")
	wait("Detach? [y/N]")
	io.WriteString(terminal, "Y\r")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestClientDetachesBeforeLocalCopyApproval(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	t.Setenv("NO_COLOR", "1")
	for _, tc := range []struct {
		name, answer string
		spawn, stay  bool
	}{
		{name: "default declines", answer: "\r"},
		{name: "interrupt cancels", answer: "\x03"},
		{name: "explicit consent from spawn", answer: "Y\r", spawn: true},
		{name: "stay locally", answer: "Y\r", stay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			command := "fixture-launch"
			if tc.spawn {
				command = "fixture-spawn"
			}
			cmd, terminal, wait := startSessionClient(t, e, []string{command, "fixture", "keys", "--server", e.bin})
			wait("Connected fixture:keys")
			source := filepath.Join(e.dir, "approval-source")
			suggested := filepath.Join(e.dir, "suggested")
			selected := filepath.Join(e.dir, "selected file")
			if err := os.WriteFile(source, []byte("approved contents"), 0600); err != nil {
				t.Fatal(err)
			}
			request := message{Type: "approval_request", SoftwareVersion: version, Operations: []operation{{
				Kind: "download", Path: source, Name: filepath.Base(source), Size: int64(len("approved contents")), LocalHint: suggested,
			}}}
			helper, err := e.dial("keys")
			if err != nil {
				t.Fatal(err)
			}
			defer helper.Close()
			_ = helper.SetDeadline(time.Now().Add(5 * time.Second))
			sendTest(t, helper, request)
			if m := recv(t, helper); m.Type != "approval_handoff" || m.SoftwareVersion != version {
				t.Fatalf("approval handoff: %+v", m)
			}
			helper.Close()

			wait("\x1b[?1049h")
			wait("From fixture / keys")
			wait("Download " + source)
			wait("Approve this request? [y/N]")
			status, err := e.dial("keys")
			if err != nil {
				t.Fatal(err)
			}
			defer status.Close()
			sendTest(t, status, message{Type: "status", Action: "health"})
			if m := recv(t, status); m.Type != "session" || m.Attached || m.PID <= 0 || m.Temporary != tc.spawn {
				t.Fatalf("source shell was not detached and alive during approval: %+v", m)
			}

			write := func(s string) {
				t.Helper()
				if _, err := io.WriteString(terminal, s); err != nil {
					t.Fatal(err)
				}
			}
			write(tc.answer)
			if tc.answer != "Y\r" {
				wait("approval cancelled")
				if _, err := os.Stat(selected); !os.IsNotExist(err) {
					t.Fatalf("cancelled destination: %v", err)
				}
			} else {
				wait("Save as:")
				wait(suggested)
				write("\x15" + selected + "\r")
				wait("Return to session after operation? [Y/n]")
				if tc.stay {
					write("n\r")
				} else {
					write("\r")
				}
				wait("Saved " + selected)
				data, err := os.ReadFile(selected)
				if err != nil || string(data) != "approved contents" {
					t.Fatalf("selected destination: %q, %v", data, err)
				}
			}
			if !tc.stay {
				wait("Connected fixture:keys")
				write("exit\n")
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}

			if _, err := os.Stat(suggested); !os.IsNotExist(err) {
				t.Fatalf("unused destination: %v", err)
			}
		})
	}
}

func TestClientFailedSwitchRestoresOriginal(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	alpha, _ := e.attach("alpha")
	detachTestClient(t, alpha)
	var fail atomic.Bool
	config := sessionSSHWithHook(t, e, "", func(_ context.Context, _ string, stream cryptossh.Channel) bool {
		if !fail.CompareAndSwap(true, false) {
			return false
		}
		sshtest.Exit(stream, 42)
		return true
	})
	cmd, terminal, wait := startSessionClientWithSSH(t, e, []string{"fixture-launch", "fixture", "keys", "--server", e.bin}, config)
	wait("Connected fixture:keys")
	// Fail exactly the next SSH launch, after preflight has accepted alpha.
	fail.Store(true)
	if _, err := terminal.Write([]byte{2, 'n'}); err != nil {
		t.Fatal(err)
	}
	wait("Target unavailable; restoring keys.")
	wait("Connected fixture:keys")
	if _, err := io.WriteString(terminal, "printf 'RESTORED-%s' input\n"); err != nil {
		t.Fatal(err)
	}
	wait("RESTORED-input")
	if _, err := terminal.Write([]byte{2, 'd'}); err != nil {
		t.Fatal(err)
	}
	wait("Detached from ")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "keys"} {
		b, _ := e.attach(name, "attach")
		b.writeInput(t, []byte("exit\n"))
		b.readExit(t)
	}
}

func reattachTestWithTree(t *testing.T, e *testEnv, name string, pid int, tree ...shellTreeNode) *bridgeConn {
	t.Helper()
	id, err := resolveSession(name)
	if err != nil {
		t.Fatal(err)
	}
	b := e.bridge(name, "attach")
	b.sendAttach(t, message{Type: "attach", Action: "existing", Instance: id, Tree: tree, Term: "xterm", Cols: 80, Rows: 24})
	if m := recv(t, b.out); m.Type != "ready" || m.PID != pid {
		t.Fatalf("reattach: %+v", m)
	}
	return b
}

func expectTerminalText(t *testing.T, b *bridgeConn, marker string) string {
	t.Helper()
	var output strings.Builder
	answeredDA, answeredCPR := 0, 0
	for i := 0; i < 100; i++ {
		output.Write(b.readOutput(t))
		// Emulate the basic xterm replies fish uses as input barriers.
		all := output.String()
		for n := strings.Count(all, "\x1b[0c"); answeredDA < n; answeredDA++ {
			b.writeInput(t, []byte("\x1b[?1;2c"))
		}
		for n := strings.Count(all, "\x1b[6n"); answeredCPR < n; answeredCPR++ {
			b.writeInput(t, []byte("\x1b[1;1R"))
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
	// Match the client: drain terminal input before detaching on control.
	sendTest(t, b.in, message{Type: "input_barrier", TerminalOffset: b.inputBytes})
	untilType(t, b.out, "input_drained")
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
	b, resumedPID := e.attach("redraw", "attach")
	if resumedPID != pid {
		t.Fatal("reattachment replaced the shell")
	}
	assertMouseReplay(t, expectTerminalText(t, b, "REDRAW-ready"), true)
	b.writeInput(t, []byte{3})
	shellOutput(t, b, "printf 'SHELL-%s' ready", "SHELL-ready")
	b.writeInput(t, []byte("exit\n"))
	b.readExit(t)
}

func assertMouseReplay(t *testing.T, output string, enabled bool) {
	t.Helper()
	if !strings.Contains(output, "\x1b[?1003l") {
		t.Fatalf("missing mouse reset: %q", output)
	}
	for _, mode := range []string{"1003", "1006", "1004", "2004"} {
		if strings.Contains(output, "\x1b[?"+mode+"h") != enabled {
			t.Fatalf("mouse %s state: %q", mode, output)
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
	b.writeInput(t, []byte("touch "+shellQuote(marker)))
	expectTerminalText(t, b, "touch ")
	detachTestClient(t, b)
	b, _ = e.attach("bashprompt", "attach")
	expectTerminalText(t, b, "READY-PROMPT> ")
	if data, err := os.ReadFile(filepath.Join(home, ".startup-count")); err != nil || string(data) != "x" {
		t.Fatalf("startup rerun: %q %v", data, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("reattach executed pending input: %v", err)
	}
	b.writeInput(t, []byte{21})
	// Keep the cursor inside the argument across reattachment. Inserting at
	// the restored position proves buffer and cursor preservation together.
	b.writeInput(t, []byte("printf 'EDIT-%s\\n' ac\x1b[D"))
	expectTerminalText(t, b, "printf ")
	detachTestClient(t, b)
	b, _ = e.attach("bashprompt", "attach")
	expectTerminalText(t, b, "READY-PROMPT> ")
	b.writeInput(t, []byte("b\n"))
	expectTerminalText(t, b, "EDIT-abc")
	shellOutput(t, b, "printf 'BASH-%s' alive", "BASH-alive")
	b.writeInput(t, []byte("exit\n"))
	b.readExit(t)
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
			b.writeInput(t, []byte{26})
			marker := stopped
			if name == "fish" {
				marker = "JOB-PROMPT> "
			}
			if output := expectTerminalText(t, b, marker); !strings.Contains(output, stopped) {
				t.Fatalf("job did not stop: %q", output)
			}
			detachTestClient(t, b)
			b, _ = e.attach("jobcontrol", "attach")
			responsiveShellOutput(t, b, "jobs -l", stopped)
			// A stopped job must remain resumable in the background and foreground.
			responsiveShellOutput(t, b, "bg; jobs", running)
			b.writeInput(t, []byte("fg\n"))
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
			b.writeInput(t, []byte{3})
			if name == "fish" {
				expectTerminalText(t, b, "JOB-PROMPT> ")
			}
			output := responsiveShellOutput(t, b, "printf 'CONTROL-%s' restored", "CONTROL-restored")
			_, tail, _ := strings.Cut(output, "CONTROL-restored")
			if name == "fish" && !strings.Contains(tail, "JOB-PROMPT> ") {
				expectTerminalText(t, b, "JOB-PROMPT> ")
			}
			b.writeInput(t, []byte("exit\n"))
			b.readExit(t)
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
			b.writeInput(t, []byte("printf '%s' ac > "+shellQuote(marker)+"; printf 'DONE-%s\\n' ready"))
			expectTerminalText(t, b, "printf")
			// Move to the beginning and then into the two-character argument.
			b.writeInput(t, []byte("\x01"+strings.Repeat("\x1b[C", len("printf '%s' a"))))
			detachTestClient(t, b)
			b, _ = e.attach("editor", "attach")
			expectTerminalText(t, b, "EDITOR-READY> ")
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("pending command executed: %v", err)
			}
			b.writeInput(t, []byte("b\n"))
			output := expectTerminalText(t, b, "DONE-ready")
			_, tail, _ := strings.Cut(output, "DONE-ready")
			if name == "fish" && !strings.Contains(tail, "EDITOR-READY> ") {
				expectTerminalText(t, b, "EDITOR-READY> ")
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "abc" {
				t.Fatalf("editor state lost: %q, %v", data, err)
			}
			b.writeInput(t, []byte("exit\n"))
			b.readExit(t)
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
	b.writeInput(t, []byte(command+"\n"))
	return expectTerminalText(t, b, marker)
}

func TestClientNestedLocalSpawnHandoff(t *testing.T) {
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startSessionClient(t, e, []string{"fixture-launch", "here:", "root", "--no-profile"})
	t.Cleanup(func() { cleanup := exec.Command(e.bin, "kill", "--all"); cleanup.Env = cmd.Env; _ = cleanup.Run() })
	wait("Connected here:root")
	wait("sshore-test> ")
	for _, name := range []string{"child", "grandchild"} {
		marker := filepath.Join(e.dir, name+"-returned")
		io.WriteString(terminal, shellQuote(e.bin)+" spawn here: --no-profile; printf done > "+shellQuote(marker)+"\n")
		wait("Connected here:")
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
	for range 2 {
		io.WriteString(terminal, "exit\n")
		wait("Connected here:")
	}
	io.WriteString(terminal, "exit\n")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestClientHandoffReturnTargetTaken(t *testing.T) {
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startSessionClient(t, e, []string{"fixture-launch", "here:", "origin", "--no-profile"})
	t.Cleanup(func() {
		cleanup := exec.Command(e.bin, "kill", "--all")
		cleanup.Env = cmd.Env
		_ = cleanup.Run()
	})
	wait("Connected here:origin")
	wait("sshore-test> ")
	io.WriteString(terminal, shellQuote(e.bin)+" launch here: --no-profile\n")
	wait("Connected here:")
	other, otherTerminal, otherWait := startSessionClient(t, e, []string{"attach", "here:origin"})
	otherWait("Connected here:origin")
	io.WriteString(terminal, "\x02d")
	wait("Detached from ")
	wait("Could not return to origin")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	io.WriteString(otherTerminal, "printf 'ORIGIN-%s' alive\n")
	otherWait("ORIGIN-alive")
	io.WriteString(otherTerminal, "exit\n")
	if err := other.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestClientHandoffHintReleasedOnFrontendExit(t *testing.T) {
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startSessionClientWithSSH(t, e, []string{"fixture-launch", "here:", "origin", "--no-profile"}, sessionSSH(t, e, ""))
	t.Cleanup(func() { cleanup := exec.Command(e.bin, "kill", "--all"); cleanup.Env = cmd.Env; _ = cleanup.Run() })
	wait("Connected here:origin")
	wait("sshore-test> ")
	io.WriteString(terminal, shellQuote(e.bin)+" spawn fixture: --no-profile --server "+shellQuote(e.bin)+"\n")
	wait("Connected fixture:")
	check := func(wantHost string) bool {
		list := exec.Command(e.bin, "list", "--json")
		list.Env = cmd.Env
		out, err := list.Output()
		if err != nil {
			return false
		}
		var inv inventory
		if json.Unmarshal(out, &inv) != nil {
			return false
		}
		for _, row := range inv.LocalShells {
			if row.Name == "origin" && row.State == "detached" && row.ConnectedHost == wantHost {
				return true
			}
		}
		return false
	}
	if !check("fixture") {
		t.Fatal("parent does not show remote handoff")
	}
	// The hint stream must outlive the daemon stream handshake deadline.
	time.Sleep(5200 * time.Millisecond)
	if !check("fixture") {
		t.Fatal("handoff hint expired while the frontend remained connected")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for !check("") {
		if time.Now().After(deadline) {
			t.Fatal("handoff hint survived frontend exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClientNestedRemoteSpawnReusesAuthentication(t *testing.T) {
	for _, command := range []string{"spawn", "launch"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			t.Setenv("SSHORE_SOCKET", "")
			e := newEnv(t)
			cmd, terminal, wait := startSessionClient(t, e, []string{"fixture-" + command, "fixture", "root", "--no-login", "--no-profile", "--server", e.bin})
			wait("Connected fixture:root")
			io.WriteString(terminal, "printf 'READY_%s\\n' shell\n")
			wait("READY_shell")
			// Any new authentication attempt now fails at SSH configuration lookup.
			ssh := localEnvironmentValue(cmd.Env, "SSHORE_SSH")
			if err := os.WriteFile(ssh, []byte("#!/bin/sh\necho unexpected-reauthentication >&2\nexit 91\n"), 0700); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				io.WriteString(terminal, shellQuote(e.bin)+" spawn --no-login --no-profile\n")
				wait("Connected fixture:")
				io.WriteString(terminal, "printf 'READY_%s\\n' shell\n")
				wait("READY_shell")
			}
			for range 2 {
				io.WriteString(terminal, "exit\n")
				wait("Connected fixture:")
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

func TestClientConsoleLaunchSurvivesParent(t *testing.T) {
	for _, host := range []string{"here:", "fixture"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			t.Setenv("SSHORE_SOCKET", "")
			e := newEnv(t)
			label := strings.TrimSuffix(host, ":")
			var sshArgs []string
			if host != "here:" {
				sshArgs = []string{"--server", e.bin}
			}
			cmd, terminal, wait := startSessionClient(t, e, append([]string{"fixture-launch", host, "root"}, sshArgs...))
			wait("Connected " + label + ":root")
			io.WriteString(terminal, "\x02:launch\r")
			wait("Connected " + label + ":")
			wait("(launch)")
			marker := filepath.Join(e.dir, "launch-id")
			io.WriteString(terminal, "printf '%s' \"${SSHORE_SOCKET:-${SSHORE_LOCAL_SESSION#task:}}\" > "+shellQuote(marker)+"; printf 'LAUNCH-%s' ready\n")
			wait("LAUNCH-ready")
			uid, err := os.ReadFile(marker)
			if err != nil || !validTaskUID(string(uid)) {
				t.Fatalf("launch identity: %q, %v", uid, err)
			}
			io.WriteString(terminal, "\x02d")
			wait("Connected " + label + ":root")
			io.WriteString(terminal, "exit\n")
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			resumed, tty, await := startSessionClient(t, e, append([]string{"attach", label + ":" + string(uid)}, sshArgs...))
			await("Connected " + label + ":")
			tty.WriteString("printf 'LAUNCH-%s' survived\n")
			await("LAUNCH-survived")
			tty.WriteString("exit\n")
			if err := resumed.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClientSpawnShortcutReturnsToParent(t *testing.T) {
	for _, tc := range []struct{ command, host string }{{"launch", "here:"}, {"spawn", "here:"}, {"launch", "fixture"}, {"spawn", "fixture"}} {
		host := tc.host
		t.Run(tc.command+"/"+host, func(t *testing.T) {
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			t.Setenv("SSHORE_SOCKET", "")
			e := newEnv(t)
			args := []string{"fixture-" + tc.command, host, "root", "--no-login", "--no-profile"}
			label := "here"
			if host != "here:" {
				args = append(args, "--server", e.bin)
				label = host
			}
			cmd, terminal, wait := startSessionClient(t, e, args)
			t.Cleanup(func() { cleanup := exec.Command(e.bin, "kill", "--all"); cleanup.Env = cmd.Env; _ = cleanup.Run() })
			wait("Connected " + label + ":root")
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
				wait("Connected " + label + ":")
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
	for _, host := range []string{"here:", "fixture"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			t.Setenv("SSHORE_SOCKET", "")
			e := newEnv(t)
			args := []string{"fixture-launch", host, "interrupt", "--no-login", "--no-profile"}
			label := "here"
			if host != "here:" {
				label = host
				args = append(args, "--server", e.bin)
			}
			cmd, terminal, wait := startSessionClient(t, e, args)
			t.Cleanup(func() { cleanup := exec.Command(e.bin, "kill", "--all"); cleanup.Env = cmd.Env; _ = cleanup.Run() })
			wait("Connected " + label + ":interrupt")
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
	var block atomic.Bool
	started := make(chan struct{})
	config := sessionSSHWithHook(t, e, "", func(_ context.Context, _ string, stream cryptossh.Channel) bool {
		if !block.CompareAndSwap(true, false) {
			return false
		}
		close(started)
		_, _ = io.Copy(io.Discard, stream)
		return true
	})
	cmd, terminal, wait := startSessionClientWithSSH(t, e, []string{"fixture-launch", "fixture", "keys", "--server", e.bin}, config)
	wait("Connected fixture:keys")
	// Block the first setup until its SSH input closes on cancellation.
	block.Store(true)
	terminal.Write([]byte{2, 'n'})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("switch never reached setup")
	}
	terminal.Write([]byte{3})
	wait("Target unavailable; restoring keys.")
	wait("Connected fixture:keys")
	io.WriteString(terminal, "printf 'RESTORED_%s\\n' input\n")
	wait("RESTORED_input")
	terminal.Write([]byte{2, 'd'})
	wait("Detached from ")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "keys"} {
		b, _ := e.attach(name, "attach")
		b.writeInput(t, []byte("exit\n"))
		b.readExit(t)
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
			cmd, terminal, wait := startSessionClient(t, e, []string{"fixture-" + command, "fixture", "crash", "--no-login", "--no-profile", "--server", e.bin})
			wait("Connected fixture:crash")
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
			identity, err := proc.StartToken(record.ManagerPID)
			if err != nil {
				t.Fatal(err)
			}
			err = daemonJSON(context.Background(), record, "/ready", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if current, err := proc.StartToken(record.ManagerPID); err != nil || current != identity {
				t.Fatalf("test daemon identity changed: %v", err)
			}
			if err := syscall.Kill(record.ManagerPID, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			wait("\x1b[=0u\x1b[>4;0m")
			wait("Disconnected from fixture:crash")
			wait("Shell state unconfirmed.")
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
	for _, host := range []string{"here:", "fixture"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("SSHORE_SOCKET", "")
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			e := newEnv(t)
			args := []string{"fixture-launch", host, "mru-detach", "--no-login", "--no-profile"}
			label := "here"
			if host != "here:" {
				args = append(args, "--server", e.bin)
				label = host
			}
			cmd, terminal, wait := startSessionClient(t, e, args)
			wait("Connected " + label + ":mru-detach")
			if _, err := io.WriteString(terminal, "\x02d"); err != nil {
				t.Fatal(err)
			}
			wait("Detached from ")
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
			if len(rows) != 1 || rows[0].Name != "mru-detach" || rows[0].Local != (host == "here:") || rows[0].Identity == "" {
				t.Fatalf("detach MRU: %+v", rows)
			}
			// No operands must reattach the same local or remote shell using MRU.
			selectorsList := [][]string{{label + ":"}, {label + ":" + taskUID(rows[0].Identity)[:2]}}
			if host != "here:" {
				selectorsList = append(selectorsList, []string{taskUID(rows[0].Identity)[:6]})
			}
			for _, selectors := range selectorsList {
				reattach := append([]string{"attach"}, selectors...)
				next, tty, await := startSessionClient(t, e, reattach)
				await("Connected " + label + ":mru-detach")
				if _, err := io.WriteString(tty, "\x02d"); err != nil {
					t.Fatal(err)
				}
				await("Detached from ")
				if err := next.Wait(); err != nil {
					t.Fatal(err)
				}
			}

		})
	}
}

// Exercise the public client through a native PTY, including its Escape timer.
func TestClientEnhancedPrefixKeysAndEscape(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startPrefixClient(t, e)
	wait("Connected fixture:keys")
	io.WriteString(terminal, "stty raw -echo; printf 'ESC_%s\\n' ready; byte=$(dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \\n'); stty sane; printf 'BYTE_%s\\n' \"$byte\"\n")
	wait("ESC_ready")
	terminal.Write([]byte{27})
	wait("BYTE_1b")
	// A Kitty prefix, release report and command can span input reads.
	for _, part := range []string{"\x1b[98;", "5:1u", "\x1b[98;5:3u", "\x1b[114;1u"} {
		io.WriteString(terminal, part)
	}
	io.WriteString(terminal, "printf 'REDRAW_%s\\n' alive\n")
	wait("REDRAW_alive")
	// xterm modifyOtherKeys uses a different encoding for the same prefix.
	io.WriteString(terminal, "\x1b[27;5;98~\x1b[100;1u")
	wait("Detached from ")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	b, _ := e.attach("keys", "attach")
	b.writeInput(t, []byte("exit\n"))
	b.readExit(t)
}

func TestClientExitNotices(t *testing.T) {
	for _, host := range []string{"here:", "fixture"} {
		for _, command := range []string{"launch", "spawn"} {
			for _, code := range []string{"0", "7"} {
				t.Run(host+"/"+command+"/"+code, func(t *testing.T) {
					t.Setenv("SSHORE_SOCKET", "")
					t.Setenv("SSHORE_LOCAL_SESSION", "")
					e := newEnv(t)
					args := []string{"fixture-" + command, host, "exit-notice", "--no-login", "--no-profile"}
					label := "here"
					if host != "here:" {
						label = host
						args = append(args, "--server", e.bin)
					}
					cmd, terminal, wait := startSessionClient(t, e, args)
					wait("Connected " + label + ":exit-notice")
					io.WriteString(terminal, "exit "+code+"\n")
					wait("Exited " + label + ":exit-notice (code " + code + ")")
					err := cmd.Wait()
					if code == "0" && err != nil {
						t.Fatal(err)
					}
					if code == "7" {
						status, ok := err.(*exec.ExitError)
						if !ok || status.ExitCode() != 7 {
							t.Fatalf("exit status: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestClientRemoteExitReason(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, _, wait := startPrefixClient(t, e)
	wait("Connected fixture:keys")
	kill := exec.Command(e.bin, "kill", "fixture:keys", "--server", e.bin)
	kill.Env = cmd.Env
	if out, err := kill.CombinedOutput(); err != nil {
		t.Fatalf("kill fixture shell: %v %s", err, out)
	}
	wait("Exited fixture:keys")
	wait("killed by sshore kill")
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed shell reported success")
	}
}

func TestClientExternalSpawnParent(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	e := newEnv(t)
	cmd, terminal, wait := startSessionClient(t, e, []string{"fixture-spawn", "here:", "external-parent", "--no-login", "--no-profile"})
	wait("Connected here:external-parent")
	ps := exec.Command(e.bin, "ps")
	ps.Env = cmd.Env
	out, err := ps.CombinedOutput()
	if err != nil {
		t.Fatalf("ps: %v %s", err, out)
	}
	name, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), fmt.Sprintf("%s (pid %d)", strings.TrimSpace(string(name)), os.Getpid())) || !strings.Contains(string(out), "external-parent") {
		t.Fatalf("missing direct parent: %s", out)
	}
	io.WriteString(terminal, "exit\n")
	wait("Exited here:external-parent")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

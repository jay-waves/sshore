//go:build integration

package tests_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
)

// The frontend runs inside the harness ConPTY's job. Start the real account
// daemon outside that job so its lifetime can span two frontend processes.
func windowsClient(t *testing.T) (context.Context, func(...string) *exec.Cmd) {
	t.Helper()
	binary := os.Getenv("SSHORE_TEST_WINDOWS_CLIENT")
	if binary == "" {
		t.Skip("set SSHORE_TEST_WINDOWS_CLIENT to the built Windows executable on an isolated runner")
	}
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SHELL", filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	command := func(args ...string) *exec.Cmd { return exec.CommandContext(ctx, binary, args...) }
	daemonCtx, stopDaemon := context.WithTimeout(context.Background(), 75*time.Second)
	t.Cleanup(stopDaemon)
	daemon := exec.CommandContext(daemonCtx, binary, "daemon")
	stderr, err := daemon.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	ready := false
	t.Cleanup(func() {
		if ready {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			stop := exec.CommandContext(ctx, binary, "kill", "--all")
			stop.Stdin = strings.NewReader("kill\n")
			if out, err := stop.CombinedOutput(); err != nil {
				t.Errorf("stop fixture daemon: %v\n%s", err, out)
			}
		}
		_ = daemon.Process.Kill()
		_ = daemon.Wait()
	})
	reader := bufio.NewReader(stderr)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasSuffix(strings.TrimSpace(line), " [info] [daemon] Daemon running.") {
		t.Fatalf("fixture daemon did not start: %q (%v)", line, err)
	}
	ready = true
	go func() { _, _ = io.Copy(os.Stderr, reader) }()
	return ctx, command
}

func runWindowsTerminal(t *testing.T, ctx context.Context, cmd *exec.Cmd, run func(func(string), terminalWait)) (string, error) {
	t.Helper()
	terminal, err := pty.Start(pty.Config{
		Path: cmd.Path, Args: cmd.Args[1:], Env: cmd.Environ(), Dir: cmd.Dir,
		Size: pty.Size{Cols: 180, Rows: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	stop := context.AfterFunc(ctx, func() { _ = terminal.Close() })
	defer stop()
	transcript := terminalDialogue(t, terminal, func(send func(string), wait terminalWait) {
		// ConPTY paints text with cursor movement and can omit the final newline.
		run(send, func(marker string, _ ...bool) string { return wait(marker, true) })
	})
	return transcript, terminal.Wait()
}

func windowsRun(t *testing.T, cmd *exec.Cmd) []byte {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", cmd.Args, err, out)
	}
	return out
}

func TestWindowsConPTY(t *testing.T) {
	ctx, command := windowsClient(t)
	t.Run("detach and reattach", func(t *testing.T) { testWindowsReattach(t, ctx, command) })
	t.Run("switch to a detached root and return to the parent", func(t *testing.T) {
		start := func(name string) *exec.Cmd {
			return command("shell", "--name", name, "--pin", "--no-profile")
		}
		if out, err := runWindowsTerminal(t, ctx, start("panel-target"), func(send func(string), wait terminalWait) {
			wait("sshore: here:panel-target", true)
			send("\x02d")
			wait("Detached from here:panel-target")
			wait("sshore on local>", true)
			send("exit\r")
		}); err != nil {
			t.Fatalf("prepare detached root: %v\n%s", err, out)
		}
		taskID := func(name string) string {
			tree := string(windowsRun(t, command("ps")))
			for _, line := range strings.Split(tree, "\n") {
				_, entry, found := strings.Cut(line, "shell/")
				id, _, _ := strings.Cut(entry, " ")
				if found && len(id) == 6 && strings.Contains(entry, "("+name+")") {
					return id
				}
			}
			t.Fatalf("shell %q missing:\n%s", name, tree)
			return ""
		}
		id := taskID("panel-target")
		if out, err := runWindowsTerminal(t, ctx, start("panel-source"), func(send func(string), wait terminalWait) {
			wait("sshore: here:panel-source", true)
			parentID := taskID("panel-source")
			send("\x02a")
			wait("sshore: here:panel-target", true)
			send("\x02s")
			wait("sshore on local>", true)
			// Selecting the parent returns to its live shell. Selecting the detached
			// root by ID creates a new switch, which ends when that shell exits.
			send("attach " + parentID + "\r")
			wait("sshore: here:panel-source", true)
			send("\x02s")
			wait("sshore on local>", true)
			send("pstree\r")
			wait("(panel-target)")
			send(id + "\r")
			wait("sshore: here:panel-target", true)
			send("exit\r")
			wait("sshore: here:panel-source", true)
			send("exit\r")
			wait("sshore on local>", true)
			send("exit\r")
		}); err != nil {
			t.Fatalf("attach and command panels: %v\n%s", err, out)
		}
	})
	t.Run("confirmed shutdown", func(t *testing.T) {
		const endpoint = "here:shutdown"
		out, err := runWindowsTerminal(t, ctx, command("shell", "--name", "shutdown", "--no-profile"), func(_ func(string), wait terminalWait) {
			wait("Connected " + endpoint)
			stop := command("kill", "--all")
			stop.Stdin = strings.NewReader("y\n")
			windowsRun(t, stop)
			wait("Now in: calling terminal")
		})
		status, exited := errors.AsType[interface {
			error
			ExitCode() int
		}](err)
		if ctx.Err() != nil || err != nil && (!exited || status.ExitCode() != 1) {
			t.Fatalf("Windows terminal client: %v\n%s", err, out)
		}
		if out := string(windowsRun(t, command("kill", "--all"))); !strings.Contains(out, "No account daemon is running.") {
			t.Fatalf("shell client restarted the daemon:\n%s", out)
		}
	})
}

func testWindowsReattach(t *testing.T, ctx context.Context, command func(...string) *exec.Cmd) {
	t.Helper()
	run := func(cmd *exec.Cmd, steps func(func(string), terminalWait)) {
		t.Helper()
		if out, err := runWindowsTerminal(t, ctx, cmd, steps); err != nil {
			t.Fatalf("Windows terminal client: %v\n%s", err, out)
		}
	}
	directory := t.TempDir()
	const endpoint = "here:conpty"
	run(command("shell", "--name", "conpty", "--pin", "--no-profile"), func(send func(string), wait terminalWait) {
		wait("Connected " + endpoint)
		send("set \"SSHORE_RESUME=retained\"\r")
		send("cd /d \"" + directory + "\"\r")
		send("echo WIN_READY=%SSHORE_RESUME%\r")
		wait("WIN_READY=retained")
		send("\x02d")
		wait("Detached from " + endpoint)
		wait("No shell attached.")
		wait("sshore on local>", true)
		send("exit\r")
	})
	tree := string(windowsRun(t, command("ps")))
	_, entry, found := strings.Cut(tree, "shell/")
	if !strings.HasPrefix(tree, "local\n") || !found || !strings.Contains(entry, " (conpty)  detached") {
		t.Fatalf("detached shell is missing from the local tree:\n%s", tree)
	}
	code, _, _ := strings.Cut(entry, " ")
	if len(code) != 6 {
		t.Fatalf("unexpected shell code %q in tree:\n%s", code, tree)
	}
	run(command("a", code), func(send func(string), wait terminalWait) {
		wait("WIN_READY=retained")
		send("echo WIN_RESUMED=%SSHORE_RESUME%:%CD%\r")
		wait("WIN_RESUMED=retained:" + directory)
		send("exit\r")
		wait("No shell attached.")
		wait("sshore on local>", true)
		send("exit\r")
	})
	if strings.Contains(string(windowsRun(t, command("ls"))), "(conpty)") {
		t.Fatal("exited shell still registered")
	}
}

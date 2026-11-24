//go:build integration

package tests_test

import (
	"bufio"
	"context"
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
	if err != nil || !strings.HasSuffix(strings.TrimSpace(line), " [daemon] [info] Daemon running.") {
		t.Fatalf("fixture daemon did not start: %q (%v)", line, err)
	}
	ready = true
	go func() { _, _ = io.Copy(os.Stderr, reader) }()
	return ctx, command
}

func runWindowsTerminal(t *testing.T, ctx context.Context, cmd *exec.Cmd, run func(func(string), terminalWait)) {
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
	if err := terminal.Wait(); err != nil {
		t.Fatalf("Windows terminal client: %v\n%s", err, transcript)
	}
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
	directory := t.TempDir()
	const endpoint = "here:conpty"
	runWindowsTerminal(t, ctx, command("shell", endpoint, "--pin", "--no-profile"), func(send func(string), wait terminalWait) {
		wait("Connected " + endpoint)
		send("set \"SSHORE_RESUME=retained\"\r")
		send("cd /d \"" + directory + "\"\r")
		send("echo WIN_READY=%SSHORE_RESUME%\r")
		wait("WIN_READY=retained")
		send("\x02d")
		wait("Detached from " + endpoint)
		wait("No shell attached.")
		wait("sshore on local>")
		send("exit\r")
	})
	runWindowsTerminal(t, ctx, command("attach", endpoint), func(send func(string), wait terminalWait) {
		wait("WIN_READY=retained")
		send("echo WIN_RESUMED=%SSHORE_RESUME%:%CD%\r")
		wait("WIN_RESUMED=retained:" + directory)
		send("exit\r")
		wait("No shell attached.")
		wait("sshore on local>")
		send("exit\r")
	})
	if strings.Contains(string(windowsRun(t, command("list"))), "(conpty)") {
		t.Fatal("exited shell still registered")
	}
}

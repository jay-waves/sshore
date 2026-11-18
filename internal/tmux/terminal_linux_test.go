package tmux_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/tmux"
)

func TestNativeTerminal(t *testing.T) {
	term, err := tmux.Start(tmux.Config{Path: "/bin/sh", Args: []string{"-c", "stty size; printf '\033[31mhello\033[0m'"}, Size: tmux.Size{Cols: 91, Rows: 27}})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	done := make(chan string, 1)
	go func() {
		data, err := io.ReadAll(term)
		if err != nil {
			t.Errorf("normal PTY EOF: %v", err)
		}
		done <- string(data)
	}()
	select {
	case out := <-done:
		if !strings.Contains(out, "27 91") || !strings.Contains(out, "\033[31mhello\033[0m") {
			t.Fatalf("unexpected output %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PTY output timed out")
	}
	if err := term.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := term.Resize(tmux.Size{}); err == nil {
		t.Fatal("accepted invalid size")
	}
}

func TestWaitConcurrent(t *testing.T) {
	term, err := tmux.Start(tmux.Config{Path: "/bin/sh", Args: []string{"-c", "exit 7"}, Size: tmux.Size{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	go io.Copy(io.Discard, term)
	results := make(chan error, 8)
	for range 8 {
		go func() { results <- term.Wait() }()
	}
	var first error
	for range 8 {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("lost exit failure")
			}
			if first == nil {
				first = err
			} else if first != err {
				t.Fatal("Wait returned different results")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Wait blocked")
		}
	}
}

func TestTerminalOutputEndsAfterShellExit(t *testing.T) {
	terminal, err := tmux.Start(tmux.Config{Path: "/bin/sh", Args: []string{"-c", "trap '' HUP; sleep 4 & printf ready; exit 0"}, Size: tmux.Size{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	done := make(chan struct {
		data []byte
		err  error
	}, 1)
	go func() {
		data, err := io.ReadAll(terminal)
		done <- struct {
			data []byte
			err  error
		}{data, err}
	}()
	select {
	case result := <-done:
		if result.err != nil || string(result.data) != "ready" {
			t.Fatalf("output %q: %v", result.data, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell exit held hostage by descendant's PTY handle")
	}
	if err := terminal.Wait(); err != nil {
		t.Fatal(err)
	}
}

func blockedInputTerminal(t *testing.T) (*tmux.Terminal, string) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "read-input")
	term, err := tmux.Start(tmux.Config{Path: "/bin/sh", Args: []string{"-c", `stty raw -echo; printf READY; while [ ! -e "$1" ]; do sleep 0.01; done; exec cat`, "mux-test", gate}, Size: tmux.Size{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close(); _ = os.WriteFile(gate, nil, 0600); _ = term.Wait() })
	ready := make(chan error, 1)
	go func() {
		data := make([]byte, 5)
		_, e := io.ReadFull(term, data)
		if e == nil && string(data) != "READY" {
			e = errors.New("missing ready marker")
		}
		ready <- e
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal startup timed out")
	}
	return term, gate
}

func TestInputLeaseStopsBlockedWriteBeforeNewOwner(t *testing.T) {
	term, gate := blockedInputTerminal(t)
	old := tmux.NewInputLease(term)
	writing := make(chan struct{})
	result := make(chan error, 1)
	go func() { close(writing); _, err := old.Write(bytes.Repeat([]byte{'O'}, 4*1024*1024)); result <- err }()
	<-writing
	// The child deliberately does not read, so this write must remain in flight.
	select {
	case err := <-result:
		t.Fatalf("large input unexpectedly completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	stopped := make(chan error, 1)
	go func() { stopped <- old.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not cancel the blocked native PTY write")
	}
	select {
	case err := <-result:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("write error %v, want deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old write survived Stop")
	}
	if n, err := old.Write([]byte("STALE")); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stopped writer accepted bytes: %d %v", n, err)
	}
	if err := old.Stop(); err != nil {
		t.Fatal(err)
	}
	// Let the child drain the accepted prefix, then transfer input to a new lease.
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	output := make(chan error, 1)
	go func() {
		r := bufio.NewReader(term)
		line, err := r.ReadBytes('\n')
		if err == nil && !bytes.Equal(bytes.TrimSuffix(line, []byte{'\n'}), bytes.Repeat([]byte{'O'}, len(line)-1)) {
			err = errors.New("stale input appeared after lease transfer")
		}
		if err == nil {
			line, err = r.ReadBytes('\n')
			if err == nil && string(line) != "NEW\n" {
				err = errors.New("new input was corrupted")
			}
		}
		output <- err
	}()
	fresh := tmux.NewInputLease(term)
	wrote := make(chan error, 1)
	go func() { _, err := fresh.Write([]byte("\nNEW\n")); wrote <- err }()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("deadline leaked to new owner: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new writer blocked")
	}
	select {
	case err := <-output:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new owner output missing")
	}
}

func TestNativeTerminalCloseInterruptsBlockedRead(t *testing.T) {
	term, _ := blockedInputTerminal(t)
	read := make(chan error, 1)
	started := make(chan struct{})
	go func() { close(started); _, err := term.Read(make([]byte, 1)); read <- err }()
	<-started
	select {
	case err := <-read:
		t.Fatalf("Read returned before close: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	closed := make(chan error, 1)
	go func() { closed <- term.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked on Read")
	}
	select {
	case err := <-read:
		if !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.EOF) {
			t.Fatalf("read error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read survived Close")
	}
}

func TestBashStartupFiles(t *testing.T) {
	path, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	home := t.TempDir()
	for file, value := range map[string]string{".bash_profile": "LOGIN", ".bashrc": "INTERACTIVE"} {
		if err := os.WriteFile(filepath.Join(home, file), []byte("export SSHORE_STARTUP_TEST="+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, noLogin := range []bool{false, true} {
		args, err := tmux.StartupArgs(path, noLogin, false)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, path, append(args, "-c", `printf '\nSTARTUP:%s\n' "$SSHORE_STARTUP_TEST"`)...)
		cmd.Env = append(os.Environ(), "HOME="+home, "SSHORE_STARTUP_TEST=", "TERM=dumb")
		out, err := cmd.CombinedOutput()
		cancel()
		want := "STARTUP:LOGIN"
		if noLogin {
			want = "STARTUP:INTERACTIVE"
		}
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("noLogin=%v: %v %s", noLogin, err, out)
		}
	}
}

func TestNoProfileStartupFiles(t *testing.T) {
	for _, name := range []string{"bash", "sh"} {
		t.Run(name, func(t *testing.T) {
			path, err := exec.LookPath(name)
			if err != nil {
				t.Skip(err)
			}
			home := t.TempDir()
			for _, file := range []string{".bash_profile", ".bashrc", ".profile", "env-hook"} {
				if err := os.WriteFile(filepath.Join(home, file), []byte("export SSHORE_PROFILE_RAN=yes\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args, err := tmux.StartupArgs(path, false, true)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, path, append(args, "-c", `printf '\nRESULT:%s:%s\n' "$SSHORE_PROFILE_RAN" "$SSHORE_INHERITED"`)...)
			source := append(os.Environ(), "HOME="+home, "TERM=dumb", "SSHORE_PROFILE_RAN=", "SSHORE_INHERITED=keep", "ENV="+filepath.Join(home, "env-hook"))
			cmd.Env = tmux.StartupEnv(path, source, true)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "RESULT::keep") {
				t.Fatalf("%v %s", err, out)
			}
			if source[len(source)-1] != "ENV="+filepath.Join(home, "env-hook") {
				t.Fatal("parent environment mutated")
			}
		})
	}
}

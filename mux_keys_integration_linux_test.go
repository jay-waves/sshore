package main

import (
	"context"
	"github.com/creack/pty"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientPrefixKeysAndDetach(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	e := newEnv(t)
	cmd, terminal, wait := startPrefixClient(t, e)
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
	cmd := exec.CommandContext(ctx, e.bin, "new", "fixture", "keys", "--server", e.bin)
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

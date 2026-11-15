package tmux

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func blockedInputTerminal(t *testing.T) (*Terminal, string) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "read-input")
	cmd := exec.Command("/bin/sh", "-c", `stty raw -echo; printf READY; while [ ! -e "$1" ]; do sleep 0.01; done; exec cat`, "mux-test", gate)
	term, err := StartCommand(cmd, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close(); _ = cmd.Process.Kill(); _ = term.Wait() })
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
	old := NewInputLease(term)
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
	fresh := NewInputLease(term)
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

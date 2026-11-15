package tmux

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestNativeTerminal(t *testing.T) {
	term, err := Start(Config{Path: "/bin/sh", Args: []string{"-c", "stty size; printf '\033[31mhello\033[0m'"}, Size: Size{Cols: 91, Rows: 27}})
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
	if err := term.Resize(Size{}); err == nil {
		t.Fatal("accepted invalid size")
	}
}

func TestWaitConcurrent(t *testing.T) {
	term, err := Start(Config{Path: "/bin/sh", Args: []string{"-c", "exit 7"}, Size: Size{Cols: 80, Rows: 24}})
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
	terminal, err := Start(Config{Path: "/bin/sh", Args: []string{"-c", "trap '' HUP; sleep 4 & printf ready; exit 0"}, Size: Size{Cols: 80, Rows: 24}})
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

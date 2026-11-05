package main

import (
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/creack/pty"
	"golang.org/x/term"
)

func TestRawTerminalRestoreDisablesMouseReporting(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	t.Setenv("TERM", "xterm-256color")
	output := slave
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = slave, output
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	restore, err := rawTerminal()
	if err != nil {
		t.Fatal(err)
	}
	// A remote TUI enables all-motion SGR reporting, then the connection
	// detaches without delivering that application's cleanup sequences.
	_, _ = output.WriteString("\x1b[?1003h\x1b[?1006h")
	restore()
	after, err := term.GetState(int(slave.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("terminal input state not restored: %v", err)
	}
	data := make([]byte, len("\x1b[?1003h\x1b[?1006h")+len(terminalInputResetSequence()))
	_, err = io.ReadFull(master, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"9", "1000", "1001", "1002", "1003", "1005", "1006", "1015", "1016", "1004", "2004"} {
		disable := strings.LastIndex(string(data), "\x1b[?"+mode+"l")
		enable := strings.LastIndex(string(data), "\x1b[?"+mode+"h")
		if disable < 0 || disable < enable {
			t.Errorf("mouse mode %s left enabled: %q", mode, data)
		}
	}
}

func TestTerminalControlSkipsPlainOutput(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t.Setenv("TERM", "xterm-256color")
	writeTerminalControl(f, "control")
	b, err := os.ReadFile(f.Name())
	if err != nil || len(b) != 0 {
		t.Fatalf("redirected control: %q %v", b, err)
	}
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	t.Setenv("TERM", "dumb")
	writeTerminalControl(slave, "control")
	_, _ = slave.WriteString("sentinel")
	b = make([]byte, 8)
	if _, err := io.ReadFull(master, b); err != nil || string(b) != "sentinel" {
		t.Fatalf("dumb control: %q %v", b, err)
	}
}

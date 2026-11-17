package tmux

import (
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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
	restore, err := RawConsole()
	if err != nil {
		t.Fatal(err)
	}
	// A remote TUI enables mouse and enhanced keyboard release reporting, then
	// disconnects without delivering its cleanup sequences.
	remoteModes := "\x1b[?1003h\x1b[?1006h\x1b[>11u\x1b[>4;2m"
	_, _ = output.WriteString(remoteModes)
	restore()
	after, err := term.GetState(int(slave.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("terminal input state not restored: %v", err)
	}
	data := make([]byte, len(remoteModes)+len(terminalInputResetSequence()))
	_, err = io.ReadFull(master, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, reset := range []string{"\x1b[=0u", "\x1b[>4;0m"} {
		if strings.Index(string(data), reset) < len(remoteModes) {
			t.Fatalf("enhanced keyboard reporting left enabled: %q", data)
		}
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

func TestTerminalKeyboardStopReleasesInput(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	received := make(chan string, 1)
	stop, err := startTerminalKeyboardFile(context.Background(), input, func(data []byte, err error) {
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		received <- string(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte("old")); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-received:
		if data != "old" {
			t.Fatal(data)
		}
	case <-time.After(time.Second):
		t.Fatal("keyboard data not delivered")
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("keyboard stop did not join")
	}
	// No old reader can steal subsequent authentication/session input.
	if _, err = writer.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3)
	if _, err = io.ReadFull(input, data); err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("next reader got %q", data)
	}
	stop()
}

func TestTerminalKeyboardParentCancelAndConcurrentStop(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stop, err := startTerminalKeyboardFile(ctx, input, func([]byte, error) { t.Error("silent keyboard emitted data") })
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(stop)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled reader remained live")
	}
}

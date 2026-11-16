//go:build windows

package tmux

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Use a child with its own console: CI normally redirects the test process's
// handles, and changing the runner's console would affect unrelated commands.
func TestWindowsMouseCaptureFollowsRemoteRequests(t *testing.T) {
	if os.Getenv("SSHORE_TEST_MOUSE_CONSOLE") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsMouseCaptureFollowsRemoteRequests$")
		cmd.Env = append(os.Environ(), "SSHORE_TEST_MOUSE_CONSOLE=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("console test: %v\n%s", err, output)
		}
		return
	}
	openConsole := func(name string) *os.File {
		t.Helper()
		path, _ := windows.UTF16PtrFromString(name)
		handle, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		f := os.NewFile(uintptr(handle), name)
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = openConsole("CONIN$"), openConsole("CONOUT$")
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	in := windows.Handle(os.Stdin.Fd())
	readMode := func() uint32 {
		t.Helper()
		var mode uint32
		if err := windows.GetConsoleMode(in, &mode); err != nil {
			t.Fatal(err)
		}
		return mode
	}
	inherited := readMode() | windows.ENABLE_MOUSE_INPUT | windows.ENABLE_QUICK_EDIT_MODE | windows.ENABLE_EXTENDED_FLAGS
	if err := windows.SetConsoleMode(in, inherited); err != nil {
		t.Fatal(err)
	}
	restore, err := RawConsole()
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if mode := readMode(); mode&windows.ENABLE_MOUSE_INPUT != 0 || mode&windows.ENABLE_VIRTUAL_TERMINAL_INPUT == 0 {
		t.Fatalf("shell inherited mouse capture: %#x", mode)
	}
	var state InputState
	for _, enabled := range []bool{true, false} {
		seq := "\x1b[?1003;1006l"
		if enabled {
			seq = "\x1b[?1003;1006h"
		}
		state.Observe([]byte(seq))
		if err := SyncMouse(&state); err != nil {
			t.Fatal(err)
		}
		if mode := readMode(); (mode&windows.ENABLE_MOUSE_INPUT != 0) != enabled || mode&windows.ENABLE_VIRTUAL_TERMINAL_INPUT == 0 {
			t.Fatalf("remote mouse enabled=%v, console mode=%#x", enabled, mode)
		}
	}
	restore()
	if got := readMode(); got != inherited {
		t.Fatalf("restore mode=%#x, want %#x", got, inherited)
	}
}

func TestNestedConsoleInput(t *testing.T) {
	if result := os.Getenv("SSHORE_TEST_NESTED_RESULT"); result != "" {
		writeTerminalControl(os.Stdout, "\x1b[?9001h")
		restore, err := RawConsole()
		if err != nil {
			t.Fatal(err)
		}
		defer restore()
		// A child ConPTY emits this request during startup.
		out, flush := OutputWriter()
		defer flush()
		if _, err := io.WriteString(out, "\x1b[?9001hSSHORE_INPUT_READY"); err != nil {
			t.Fatal(err)
		}
		chunks := make(chan []byte, 64)
		stop, err := StartKeyboard(context.Background(), func(b []byte, err error) {
			if len(b) > 0 {
				chunks <- b
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		var data []byte
		select {
		case data = <-chunks:
		case <-time.After(5 * time.Second):
			t.Fatal("no input")
		}
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case b := <-chunks:
				data = append(data, b...)
			case <-timer.C:
				if err := os.WriteFile(result, data, 0600); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
	for _, tc := range []struct{ name, input, want string }{
		{"vt", "\x1b[A\x1b[B\x1b[C\x1b[D", "\x1b[A\x1b[B\x1b[C\x1b[D"},
		{"win32", "\x1b[38;72;0;1;256;1_", "\x1b[A"},
		{"cursor reply", "\x1b[12;34R", "\x1b[12;34R"},
		{"unicode", "中文😀", "中文😀"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := t.TempDir() + "/input"
			term, err := Start(Config{Path: os.Args[0], Args: []string{"-test.run=^TestNestedConsoleInput$"}, Env: append(os.Environ(), "SSHORE_TEST_NESTED_RESULT="+result), Size: Size{Cols: 100, Rows: 30}})
			if err != nil {
				t.Fatal(err)
			}
			defer term.Close()
			done := make(chan string, 1)
			go func() {
				var transcript strings.Builder
				buf := make([]byte, 4096)
				sent := false
				for {
					n, err := term.Read(buf)
					transcript.Write(buf[:n])
					if !sent && strings.Contains(transcript.String(), "SSHORE_INPUT_READY") {
						sent = true
						_, _ = io.WriteString(term, tc.input)
					}
					if err != nil {
						done <- transcript.String()
						return
					}
				}
			}()
			select {
			case output := <-done:
				if err := term.Wait(); err != nil {
					t.Fatalf("%v: %q", err, output)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("nested console timeout")
			}
			data, err := os.ReadFile(result)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("upstream=%q forwarded=%q", tc.input, data)
			if string(data) != tc.want {
				t.Fatalf("input changed: got %q want %q", data, tc.want)
			}
		})
	}
}

type synchronousKeyboardPipe struct {
	handle  windows.Handle
	started chan struct{}
	once    sync.Once
}

func (r *synchronousKeyboardPipe) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	var n uint32
	err := windows.ReadFile(r.handle, p, &n, nil)
	return int(n), err
}

func TestTerminalKeyboardCancelsSynchronousRead(t *testing.T) {
	var read, write windows.Handle
	if err := windows.CreatePipe(&read, &write, nil, 4096); err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(read)
	defer windows.CloseHandle(write)
	reader := &synchronousKeyboardPipe{handle: read, started: make(chan struct{})}
	stop, err := startTerminalKeyboardReader(context.Background(), func([]byte, error) { t.Error("cancelled read emitted data") }, reader)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("synchronous keyboard cancellation did not join")
	}
	// Cancellation did not close the shared input handle.
	var n uint32
	if err = windows.WriteFile(write, []byte("next"), &n, nil); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if err = windows.ReadFile(read, buf, &n, nil); err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "next" {
		t.Fatalf("next reader got %q", buf[:n])
	}
	stop()
}

func TestTerminalKeyboardAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop, err := StartKeyboard(ctx, func([]byte, error) { t.Error("read after cancelled start") })
	if err != nil {
		t.Fatal(err)
	}
	stop()
}

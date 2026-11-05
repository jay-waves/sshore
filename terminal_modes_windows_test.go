package main

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

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
	restore, err := rawTerminal()
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if mode := readMode(); mode&windows.ENABLE_MOUSE_INPUT != 0 || mode&windows.ENABLE_VIRTUAL_TERMINAL_INPUT == 0 {
		t.Fatalf("shell inherited mouse capture: %#x", mode)
	}
	var state terminalMouseState
	for _, enabled := range []bool{true, false} {
		seq := "\x1b[?1003;1006l"
		if enabled {
			seq = "\x1b[?1003;1006h"
		}
		state.observe([]byte(seq))
		if err := syncTerminalMouse(&state); err != nil {
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

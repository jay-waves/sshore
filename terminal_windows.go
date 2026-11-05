package main

// Windows input uses ReadConsole directly; consoleInput decodes segmented UTF-16
// and preserves control keys such as Ctrl-Z. CI runs native Windows race tests,
// but full interactive Windows Terminal behavior still needs manual testing;
// cross-compilation alone does not validate console interaction.

import (
	"fmt"
	"golang.org/x/sys/windows"
	"golang.org/x/term"
	"io"
	"os"
)

func rawTerminal() (func(), error) {
	in, out := windows.Handle(os.Stdin.Fd()), windows.Handle(os.Stdout.Fd())
	var oldIn, oldOut uint32
	if err := windows.GetConsoleMode(in, &oldIn); err != nil {
		return nil, err
	}
	if err := windows.GetConsoleMode(out, &oldOut); err != nil {
		return nil, err
	}
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	restore := func() {
		resetTerminalInputModes()
		_ = term.Restore(int(os.Stdin.Fd()), state)
		_ = windows.SetConsoleMode(in, oldIn)
		_ = windows.SetConsoleMode(out, oldOut)
	}
	var mode uint32
	if err = windows.GetConsoleMode(in, &mode); err == nil {
		err = windows.SetConsoleMode(in, (mode|windows.ENABLE_VIRTUAL_TERMINAL_INPUT|windows.ENABLE_EXTENDED_FLAGS)&^windows.ENABLE_QUICK_EDIT_MODE)
	}
	if err == nil {
		err = windows.SetConsoleMode(out, oldOut|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)
	}
	if err != nil {
		restore()
		return nil, fmt.Errorf("Windows VT-capable console required: %w", err)
	}
	return restore, nil
}

func terminalInputReader() io.Reader {
	return &consoleInput{readUnits: func(b []uint16) (int, error) {
		var n uint32
		err := windows.ReadConsole(windows.Handle(os.Stdin.Fd()), &b[0], uint32(len(b)), &n, nil)
		return int(n), err
	}}
}

func supportsANSI(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil && mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0
}

// Approval runs before rawTerminal enables VT output. Temporarily enable it
// for cleanup on Windows Terminal/ConPTY and VT-capable Console Host.
func writeTerminalControl(f *os.File, sequence string) {
	h := windows.Handle(f.Fd())
	var old uint32
	if windows.GetConsoleMode(h, &old) != nil {
		return
	}
	if windows.SetConsoleMode(h, old|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) != nil {
		return
	}
	defer windows.SetConsoleMode(h, old)
	_, _ = f.WriteString(sequence)
}

package main

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

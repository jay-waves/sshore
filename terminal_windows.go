package main

// Windows input uses ReadConsole directly; consoleInput decodes segmented UTF-16
// and preserves control keys such as Ctrl-Z. CI runs native Windows race tests,
// but full interactive Windows Terminal behavior still needs manual testing;
// cross-compilation alone does not validate console interaction.

import (
	"fmt"
	"golang.org/x/sys/windows"
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
	restore := func() {
		resetTerminalInputModes()
		_ = windows.SetConsoleMode(in, oldIn)
		_ = windows.SetConsoleMode(out, oldOut)
	}
	// Do not inherit native mouse capture. With Quick Edit off, ConPTY
	// advertises ENABLE_MOUSE_INPUT to the outer terminal as mouse reporting,
	// even when the remote shell never requested it (with or without a mux).
	// Enter raw mode in one operation so no transient raw+mouse mode is exposed.
	mode := (oldIn | windows.ENABLE_VIRTUAL_TERMINAL_INPUT | windows.ENABLE_EXTENDED_FLAGS) &^
		(windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT |
			windows.ENABLE_QUICK_EDIT_MODE | windows.ENABLE_MOUSE_INPUT)
	err := windows.SetConsoleMode(in, mode)
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

// ReadConsole consumes VT text, but ConPTY multiplexers also consult the native
// mouse flag. Keep that flag aligned with the remote application's requests.
func syncTerminalMouse(state *terminalMouseState) error {
	in := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(in, &mode); err != nil {
		return err
	}
	desired := mode &^ windows.ENABLE_MOUSE_INPUT
	for _, enabled := range state.enabled[:5] {
		if enabled {
			desired |= windows.ENABLE_MOUSE_INPUT
			break
		}
	}
	if desired == mode {
		return nil
	}
	return windows.SetConsoleMode(in, desired)
}

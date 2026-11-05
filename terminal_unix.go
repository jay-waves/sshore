//go:build !windows

package main

import (
	"golang.org/x/term"
	"io"
	"os"
)

func rawTerminal() (func(), error) {
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() {
		resetTerminalInputModes()
		_ = term.Restore(fd, state)
	}, nil
}

func terminalInputReader() io.Reader { return os.Stdin }

func supportsANSI(f *os.File) bool { return true }

func writeTerminalControl(f *os.File, sequence string) {
	if os.Getenv("TERM") == "dumb" || !term.IsTerminal(int(f.Fd())) {
		return
	}
	_, _ = f.WriteString(sequence)
}

func syncTerminalMouse(state *terminalMouseState) error { return nil }

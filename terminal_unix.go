//go:build !windows

package main

import (
	shellmux "github.com/jay-waves/sshore/internal/mux"
	"io"
	"os"

	"golang.org/x/term"
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

func writeTerminalControl(f *os.File, sequence string) {
	if os.Getenv("TERM") == "dumb" || !term.IsTerminal(int(f.Fd())) {
		return
	}
	_, _ = f.WriteString(sequence)
}

func syncTerminalMouse(state *shellmux.InputState) error { return nil }

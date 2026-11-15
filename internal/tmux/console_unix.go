//go:build !windows

package tmux

import (
	"io"
	"os"

	"golang.org/x/term"
)

func RawConsole() (func(), error) {
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() {
		ResetInputModes()
		_ = term.Restore(fd, state)
	}, nil
}

func terminalInputReader() io.Reader { return os.Stdin }

func OutputWriter() (io.Writer, func() error) {
	return os.Stdout, func() error { return nil }
}

func writeTerminalControl(f *os.File, sequence string) {
	if os.Getenv("TERM") == "dumb" || !term.IsTerminal(int(f.Fd())) {
		return
	}
	_, _ = f.WriteString(sequence)
}

func SyncMouse(state *InputState) error { return nil }

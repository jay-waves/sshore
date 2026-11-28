//go:build linux || darwin || freebsd

package termui

import (
	"os"

	"github.com/chzyer/readline"
	"golang.org/x/term"
)

func enableConsoleOutput(*os.File) (func() error, error) {
	return func() error { return nil }, nil
}

func configureInputTerminal(config *readline.Config) (func() error, bool) {
	fd := int(os.Stdin.Fd())
	var state *term.State
	config.FuncMakeRaw = func() error {
		if state != nil || !term.IsTerminal(fd) {
			return nil
		}
		var err error
		state, err = term.MakeRaw(fd)
		return err
	}
	config.FuncExitRaw = func() error {
		if state == nil {
			return nil
		}
		err := term.Restore(fd, state)
		state = nil
		return err
	}
	return func() error { return nil }, true
}

func supportsANSI(f *os.File) bool { return true }

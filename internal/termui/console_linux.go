//go:build linux

package termui

import (
	"os"

	"github.com/chzyer/readline"
)

func configureInputTerminal(config *readline.Config) (func() error, bool) {
	return func() error { return nil }, true
}

func supportsANSI(f *os.File) bool { return true }

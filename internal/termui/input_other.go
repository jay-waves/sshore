//go:build !windows

package termui

import "github.com/chzyer/readline"

func configureInputTerminal(config *readline.Config) (func() error, bool) {
	return func() error { return nil }, true
}

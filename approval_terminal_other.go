//go:build !windows

package main

import "github.com/chzyer/readline"

func configureApprovalTerminal(config *readline.Config) (func() error, bool) {
	return func() error { return nil }, true
}

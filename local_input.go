package main

import (
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/tmux"
	"golang.org/x/term"
	"os"
)

func localPath(b []byte) (string, error)       { return termui.ParsePath(b) }
func newApprovalInput() (*termui.Input, error) { return termui.NewInput() }

func newLocalInput() (*termui.Input, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil, usage("local selection requires an interactive terminal; specify the local endpoint")
	}
	tmux.ResetInputModes()
	if err := tmux.FlushInput(); err != nil {
		return nil, err
	}
	return termui.NewInput()
}

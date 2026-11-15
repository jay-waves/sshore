package main

import (
	"os/exec"

	"github.com/jay-waves/sshore/internal/tmux"
)

func localShellCommand(env []string) (string, []string) {
	path := localEnvironmentValue(env, "SHELL")
	if path == "" {
		path = "/bin/sh"
	}
	return path, []string{"-i"}
}

func redrawLocalTerminal(t *tmux.Terminal) { tmux.RequestRedraw(t, t.PID()) }

func detachLocalWorker(cmd *exec.Cmd) { detachResourceWorker(cmd) }

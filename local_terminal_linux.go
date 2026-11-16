package main

import (
	"os/exec"

	"github.com/jay-waves/sshore/internal/tmux"
)

func localShellPath(env []string) string {
	path := localEnvironmentValue(env, "SHELL")
	if path == "" {
		path = "/bin/sh"
	}
	return path
}

func redrawLocalTerminal(t *tmux.Terminal) { tmux.RequestRedraw(t, t.PID()) }

func detachLocalDaemon(cmd *exec.Cmd) { detachResourceWorker(cmd) }

package main

import (
	shellmux "github.com/jay-waves/sshore/internal/mux"
	"os/exec"
)

func localShellCommand(env []string) (string, []string) {
	path := localEnvironmentValue(env, "SHELL")
	if path == "" {
		path = "/bin/sh"
	}
	return path, []string{"-i"}
}

func redrawLocalTerminal(t *shellmux.Terminal) { shellmux.RequestRedraw(t, t.PID()) }

func detachLocalWorker(cmd *exec.Cmd) { detachResourceWorker(cmd) }

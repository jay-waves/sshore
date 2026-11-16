package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/jay-waves/sshore/internal/tmux"
)

func localShellCommand(env []string) (string, []string) {
	path := localEnvironmentValue(env, "SHELL")
	if path == "" {
		path = localEnvironmentValue(env, "COMSPEC")
	}
	if path == "" {
		path = "cmd.exe"
	}
	return path, nil
}

func redrawLocalTerminal(t *tmux.Terminal) { tmux.RequestRedraw(t, t.PID()) }

func detachLocalDaemon(cmd *exec.Cmd) {
	// The worker only uses pipes and sockets; the shell gets its own ConPTY.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

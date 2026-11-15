package main

import (
	shellmux "github.com/jay-waves/sshore/internal/mux"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
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

func redrawLocalTerminal(t *shellmux.Terminal) { shellmux.RequestRedraw(t, t.PID()) }

func detachLocalWorker(cmd *exec.Cmd) {
	// The worker only uses pipes and sockets; the shell gets its own ConPTY.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

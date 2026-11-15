//go:build windows

package transport

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"syscall"
)

func IsolateCommand(cmd *exec.Cmd, background bool) {
	// Background workers own a hidden console. Keep SSH attached to it so its
	// console-subsystem askpass children inherit it instead of opening a window.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP, HideWindow: background}
}

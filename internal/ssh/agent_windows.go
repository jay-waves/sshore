package ssh

import (
	"context"
	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"net"
	"os/exec"
	"syscall"
)

func connectAgent(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, path)
}
func defaultAgent() string { return `\\.\pipe\openssh-ssh-agent` }
func hideConfigCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

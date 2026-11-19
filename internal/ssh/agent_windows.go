package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func connectAgent(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, path)
}
func defaultAgent() string { return `\\.\pipe\openssh-ssh-agent` }

func hideConfigCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// A joined list of drive-qualified paths can fail the whole-filename probe
// with ERROR_INVALID_NAME because it contains a second drive colon.
func isInvalidFilename(err error) bool { return errors.Is(err, windows.ERROR_INVALID_NAME) }

func (c *connectionConfig) expandSystemPath(path string) (string, error) {
	const token = "__PROGRAMDATA__"
	if len(path) < len(token) || !strings.EqualFold(path[:len(token)], token) {
		return path, nil
	}
	directory := environment(c.options.Environment, "ProgramData")
	if directory == "" {
		return "", fmt.Errorf("SSH environment variable ProgramData is not set")
	}
	return directory + path[len(token):], nil
}

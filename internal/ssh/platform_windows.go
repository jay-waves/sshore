package ssh

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

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

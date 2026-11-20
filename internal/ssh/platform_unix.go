//go:build linux || darwin || freebsd

package ssh

import (
	"os/exec"
)

func hideConfigCommand(cmd *exec.Cmd) {}

func isInvalidFilename(error) bool { return false }

func (c *connectionConfig) expandSystemPath(path string) (string, error) { return path, nil }

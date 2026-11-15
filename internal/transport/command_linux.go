//go:build linux

package transport

import (
	"os/exec"
)

// Keep /dev/tty available for authentication while ignoring terminal INT/QUIT.
// Startup cancellation still kills the owned subprocess. No arguments enter shell code.
func IsolateCommand(cmd *exec.Cmd, background bool) {
	args := append([]string{cmd.Path}, cmd.Args[1:]...)
	cmd.Path = "/bin/sh"
	cmd.Args = append([]string{"sh", "-c", `trap '' INT QUIT; exec "$@"`, "sshore-ftp-ssh"}, args...)
}

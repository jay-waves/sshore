package tmux

import (
	"golang.org/x/sys/unix"
	"os"
)

func FlushInput() error {
	return unix.IoctlSetInt(int(os.Stdin.Fd()), unix.TCFLSH, unix.TCIFLUSH)
}

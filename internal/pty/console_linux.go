package pty

import (
	"os"

	"golang.org/x/sys/unix"
)

func FlushInput() error {
	return unix.IoctlSetInt(int(os.Stdin.Fd()), unix.TCFLSH, unix.TCIFLUSH)
}

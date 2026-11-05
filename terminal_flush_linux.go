package main

import (
	"golang.org/x/sys/unix"
	"os"
)

func flushTerminalInput() error {
	return unix.IoctlSetInt(int(os.Stdin.Fd()), unix.TCFLSH, unix.TCIFLUSH)
}

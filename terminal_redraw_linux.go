package main

import (
	"golang.org/x/sys/unix"
	"os"
)

// Reattachment at the same size does not generate a kernel SIGWINCH. Ask the
// current foreground job (not the session's shell PID) to redraw after output
// is enabled. This is best effort: a job may exit or ignore SIGWINCH.
func requestTerminalRedraw(master *os.File) {
	group, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP)
	if err == nil && group > 0 {
		_ = unix.Kill(-group, unix.SIGWINCH)
	}
}

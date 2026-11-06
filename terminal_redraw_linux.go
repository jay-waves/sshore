package main

// Reattachment restores mouse modes, then requests SIGWINCH for the PTY
// foreground process group, even at the same size. Approved attachment waits for
// output to resume; --stay never requests redraw. Applications/multiplexers may
// ignore this or omit mode reinitialization. Tests cover state and notification,
// not every emulator's rendering; users may need their application's redraw key.

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

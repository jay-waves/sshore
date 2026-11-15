package main

import (
	"os"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// The caller holds the session locks while output is enabled. Only nudge the
// shell's dimensions; foreground applications receive their usual SIGWINCH.
func requestTerminalRedraw(master *os.File, shellPID int) {
	group, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP)
	if err != nil || group <= 0 {
		return
	}
	if group == shellPID {
		size, e := pty.GetsizeFull(master)
		if e == nil && size.Cols > 1 {
			changed := *size
			changed.Cols--
			if pty.Setsize(master, &changed) == nil {
				// Readline ignores SIGWINCH when the dimensions have not changed. Give
				// the shell a real resize, then restore it without injecting a keystroke.
				time.Sleep(30 * time.Millisecond)
				current, e := pty.GetsizeFull(master)
				if e == nil && *current == changed {
					_ = pty.Setsize(master, size)
				}
				return
			}
		}
	}
	_ = unix.Kill(-group, unix.SIGWINCH)
}

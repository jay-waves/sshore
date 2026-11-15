package tmux

import (
	"time"

	"golang.org/x/sys/unix"
)

// The caller holds the session locks while output is enabled. Only nudge the
// shell's dimensions; foreground applications receive their usual SIGWINCH.
func RequestRedraw(master *Terminal, shellPID int) {
	group, err := master.ForegroundGroup()
	if err != nil || group <= 0 {
		return
	}
	if group == shellPID {
		size, e := master.winsize()
		if e == nil && size.Col > 1 {
			changed := *size
			changed.Col--
			if master.setWinsize(&changed) == nil {
				// Readline ignores SIGWINCH when the dimensions have not changed. Give
				// the shell a real resize, then restore it without injecting a keystroke.
				time.Sleep(30 * time.Millisecond)
				current, e := master.winsize()
				if e == nil && *current == changed {
					_ = master.setWinsize(size)
				}
				return
			}
		}
	}
	_ = unix.Kill(-group, unix.SIGWINCH)
}

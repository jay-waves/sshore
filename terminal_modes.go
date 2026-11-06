package main

// At connection boundaries, clear mouse/focus reporting and bracketed paste
// before restoring local input settings. Unix sends cleanup only to a TTY whose
// TERM is not dumb; Windows temporarily enables VT and restores console flags.
// During connection, remote PTY bytes pass through unchanged.

import "os"

// Mouse reporting belongs to the terminal emulator, not termios/console raw
// mode. A remote program can disable it while detached, when its output is
// discarded. Clear it at connection boundaries so mouse motion cannot become
// input to the local approval prompt or a resumed remote shell.
// Keep this a fixed input-mode cleanup, not a screen/keyboard emulator.
func terminalInputResetSequence() string {
	return mouseResetSequence() + "\x1b[?1004l\x1b[?2004l"
}

func resetTerminalInputModes() {
	writeTerminalControl(os.Stdout, terminalInputResetSequence())
}

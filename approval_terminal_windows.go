package main

import (
	"os"

	"github.com/chzyer/readline"
	"golang.org/x/sys/windows"
)

func configureApprovalTerminal(config *readline.Config) {
	handle := windows.Handle(os.Stdin.Fd())
	configureApprovalConsole(config,
		func(mode *uint32) error { return windows.GetConsoleMode(handle, mode) },
		func(mode uint32) error { return windows.SetConsoleMode(handle, mode) })
}

// Use the checked Windows API wrappers instead of readline's syscall wrapper,
// which returns a non-nil error containing errno 0 after a successful restore.
func configureApprovalConsole(config *readline.Config, get func(*uint32) error, set func(uint32) error) {
	var saved uint32
	active := false
	config.FuncMakeRaw = func() error {
		if active {
			return nil
		}
		if err := get(&saved); err != nil {
			return err
		}
		if err := set(saved &^ (windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT)); err != nil {
			return err
		}
		active = true
		return nil
	}
	config.FuncExitRaw = func() error {
		if !active {
			return nil
		}
		if err := set(saved); err != nil {
			return err
		}
		active = false
		return nil
	}
}

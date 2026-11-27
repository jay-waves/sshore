//go:build windows

package termui

import (
	"os"

	"github.com/chzyer/readline"
	"golang.org/x/sys/windows"
)

func configureInputTerminal(config *readline.Config) (func() error, bool) {
	handle := windows.Handle(os.Stdin.Fd())
	configureInputConsole(config,
		func(mode *uint32) error { return windows.GetConsoleMode(handle, mode) },
		func(mode uint32) error { return windows.SetConsoleMode(handle, mode) })
	out := windows.Handle(os.Stdout.Fd())
	return configureInputVT(config,
		func(mode *uint32) error { return windows.GetConsoleMode(out, mode) },
		func(mode uint32) error { return windows.SetConsoleMode(out, mode) })
}

// Bypass readline's legacy ANSI translator only after native VT is enabled.
// Both editor writers use the same console, whose mode is restored on Close.
func configureInputVT(config *readline.Config, get func(*uint32) error, set func(uint32) error) (func() error, bool) {
	restore, err := enableConsoleVT(get, set)
	if err != nil {
		return func() error { return nil }, false
	}
	config.Stdout, config.Stderr = os.Stdout, os.Stdout
	return restore, true
}

func enableLocalConsoleOutput() (func() error, error) {
	out := windows.Handle(os.Stdout.Fd())
	return enableConsoleVT(func(mode *uint32) error { return windows.GetConsoleMode(out, mode) },
		func(mode uint32) error { return windows.SetConsoleMode(out, mode) })
}

func enableConsoleVT(get func(*uint32) error, set func(uint32) error) (func() error, error) {
	var saved uint32
	if err := get(&saved); err != nil {
		return nil, err
	}
	if err := set(saved | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return nil, err
	}
	return func() error { return set(saved) }, nil
}

// Use the checked Windows API wrappers instead of readline's syscall wrapper,
// which returns a non-nil error containing errno 0 after a successful restore.
func configureInputConsole(config *readline.Config, get func(*uint32) error, set func(uint32) error) {
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

func supportsANSI(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil && mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0
}

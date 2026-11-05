package main

import (
	"golang.org/x/sys/windows"
	"os"
)

func flushTerminalInput() error {
	return windows.FlushConsoleInputBuffer(windows.Handle(os.Stdin.Fd()))
}

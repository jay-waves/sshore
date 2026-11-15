package tmux

import (
	"golang.org/x/sys/windows"
	"os"
)

func FlushInput() error {
	return windows.FlushConsoleInputBuffer(windows.Handle(os.Stdin.Fd()))
}

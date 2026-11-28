package termui

import (
	"context"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func openLFPath(_ context.Context, name string) error {
	name, err := filepath.Abs(name)
	if err != nil {
		return err
	}
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	// A nil verb uses the file association's default action.
	return windows.ShellExecute(0, nil, path, nil, nil, windows.SW_SHOWNORMAL)
}

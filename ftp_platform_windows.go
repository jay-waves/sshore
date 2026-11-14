//go:build windows

package main

import (
	"errors"
	"fmt"
	"github.com/winfsp/go-winfsp"
	"golang.org/x/sys/windows"
	"sync"
)

const winFspInstallURL = "https://winfsp.dev/"

var winFspCheckOnce sync.Once
var winFspCheckError error

func checkFTPPlatform() error {
	winFspCheckOnce.Do(func() { winFspCheckError = checkWinFsp(winfsp.BinPath, winfsp.LoadWinFSP) })
	return winFspCheckError
}
func checkWinFsp(find func() (string, error), load func() error) error {
	if _, e := find(); e != nil {
		if errors.Is(e, windows.ERROR_FILE_NOT_FOUND) || errors.Is(e, windows.ERROR_PATH_NOT_FOUND) {
			return fmt.Errorf("WinFsp was not found. Download and install it from %s, then retry: %w", winFspInstallURL, e)
		}
		return fmt.Errorf("cannot inspect the WinFsp installation: %w", e)
	}
	if e := load(); e != nil {
		return fmt.Errorf("WinFsp is installed but could not be loaded; repair the installation from %s, then retry: %w", winFspInstallURL, e)
	}
	return nil
}

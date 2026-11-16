//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

func detachResourceWorker(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
}
func lockResourceState(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
func prepareFTPPaths(o *ftpOptions) (func(), error) {
	cleanup := func() {}
	if o.mountpoint != "" {
		if len(o.mountpoint) == 2 && o.mountpoint[1] == ':' {
			return cleanup, nil
		}
		p, e := filepath.Abs(o.mountpoint)
		o.mountpoint = p
		return cleanup, e
	}
	drives, e := windows.GetLogicalDrives()
	if e != nil {
		return cleanup, e
	}
	for c := byte('Z'); c >= 'D'; c-- {
		if drives&(1<<uint(c-'A')) == 0 {
			o.mountpoint = string(c) + ":"
			return cleanup, nil
		}
	}
	return cleanup, fmt.Errorf("no free drive letter; specify a mountpoint")
}

func resourceLockBusy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }

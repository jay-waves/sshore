//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

func privateState(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("approval state must be owned by you, private, and not a symlink: %s", path)
	}
	return nil
}

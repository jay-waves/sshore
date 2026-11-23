//go:build !windows

package sshconfig

import (
	"fmt"
	"os"
	"syscall"
)

func checkPermissions(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		owner := int(info.Sys().(*syscall.Stat_t).Uid)
		if owner != 0 && owner != os.Getuid() || info.Mode().Perm()&022 != 0 {
			return fmt.Errorf("%s: unsafe owner or permissions for SSH config", file.Name())
		}
	}
	return nil
}

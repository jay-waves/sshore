//go:build linux || (darwin && arm64)

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/jay-waves/sshore/internal/files"
)

func prepareFTPPaths(o *ftpOptions) (func(), error) {
	cleanup := func() {}
	if o.Mountpoint != "" {
		p, e := absoluteLocalPath(o.Mountpoint)
		o.Mountpoint = p
		return cleanup, e
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return cleanup, e
	}
	root := filepath.Join(home, "mnt")
	if e = os.MkdirAll(root, 0700); e != nil {
		return cleanup, e
	}
	// ~/mnt may already be shared for other user-managed mounts (mode 0755).
	// Require ownership and prevent other users from replacing mount directories.
	info, e := os.Lstat(root)
	if e != nil {
		return cleanup, e
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return cleanup, fmt.Errorf("mount parent must be a directory owned by you and not writable by others: %s", root)
	}
	o.Mountpoint = filepath.Join(root, o.Name)
	e = os.Mkdir(o.Mountpoint, 0700)
	if os.IsExist(e) {
		return cleanup, files.ValidateMountpoint(o.Mountpoint)
	}
	if e != nil {
		return cleanup, e
	}
	return func() { _ = os.Remove(o.Mountpoint) }, nil
}

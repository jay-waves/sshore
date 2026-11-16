//go:build !windows

package state

import (
	"fmt"
	"os"
	"syscall"
)

// Private validates ownership, permissions and path type without following symlinks.
func Private(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("state path must be owned by you, private, and not a symlink: %s", path)
	}
	return nil
}

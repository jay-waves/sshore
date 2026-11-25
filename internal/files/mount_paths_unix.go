//go:build linux || (darwin && arm64)

package files

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func defaultMountpoint(name string) (string, func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}
	root := filepath.Join(home, "mnt")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", nil, err
	}
	// ~/mnt may already be shared for other user-managed mounts (mode 0755).
	// Require ownership and prevent other users from replacing mount directories.
	info, err := os.Lstat(root)
	if err != nil {
		return "", nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return "", nil, fmt.Errorf("mount parent must be a directory owned by you and not writable by others: %s", root)
	}
	for {
		leaf := name
		if leaf == "" {
			var nonce [3]byte
			rand.Read(nonce[:])
			leaf = hex.EncodeToString(nonce[:])
		}
		path := filepath.Join(root, leaf)
		err := os.Mkdir(path, 0700)
		if err == nil {
			return path, func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
		if name != "" {
			if err := ValidateMountpoint(path); err != nil {
				return "", nil, err
			}
			return path, func() {}, nil
		}
		// Each foreground mount owns its directory.
	}
}

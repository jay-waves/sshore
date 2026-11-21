package files

import (
	"fmt"
	"os"
)

func CheckMountPlatform() error {
	const helper = "/Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse"
	if _, err := os.Stat(helper); err != nil {
		return fmt.Errorf("macFUSE mount helper unavailable; install macFUSE from https://macfuse.github.io/: %w", err)
	}
	return nil
}

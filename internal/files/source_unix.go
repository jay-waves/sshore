//go:build linux || (darwin && arm64)

package files

import (
	"os"

	"golang.org/x/sys/unix"
)

func openSourceFile(path string) (*os.File, error) {
	// Nonblocking open prevents a replacement FIFO from waiting for a writer
	// before OpenSource can reject its type. The returned handle never follows
	// a replacement symlink and cannot leak across exec.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

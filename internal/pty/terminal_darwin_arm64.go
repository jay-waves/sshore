package pty

import (
	"os"
	"os/exec"
	"time"

	nativepty "github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func openTerminalPair() (*os.File, *os.File, error) {
	original, slave, err := nativepty.Open()
	if err != nil {
		return nil, nil, err
	}
	defer original.Close()

	// The dependency opens a blocking master. Register the nonblocking
	// duplicate with Go's I/O poller at construction to enable deadlines.
	fd, err := unix.FcntlInt(original.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		_ = slave.Close()
		return nil, nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		_ = slave.Close()
		return nil, nil, err
	}
	master := os.NewFile(uintptr(fd), original.Name())
	if err := master.SetWriteDeadline(time.Time{}); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func waitTerminalChild(cmd *exec.Cmd) error { return cmd.Wait() }

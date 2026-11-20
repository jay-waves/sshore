package pty

import (
	"os"
	"os/exec"
	"strconv"

	"github.com/jay-waves/sshore/internal/proc"
	"golang.org/x/sys/unix"
)

func openTerminalPair() (master, slave *os.File, err error) {
	// Open the master nonblocking so deadlines can interrupt terminal I/O.
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	closeMaster := master.Close
	defer func() {
		if err != nil {
			_ = closeMaster()
		}
	}()
	var number int
	var ioctlErr error
	control, err := master.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	if err := control.Control(func(fd uintptr) {
		number, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPTN)
		if ioctlErr == nil {
			ioctlErr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0)
		}
	}); err != nil {
		return nil, nil, err
	}
	if ioctlErr != nil {
		return nil, nil, ioctlErr
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|unix.O_NOCTTY, 0)
	return master, slave, err
}

func waitTerminalChild(cmd *exec.Cmd) error { return proc.Wait(cmd) }

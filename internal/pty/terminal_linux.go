package pty

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// waitTerminalChild parks in Go's I/O poller until a started child exits, then reaps it.
// The caller must own the command's single Wait call.
func (t *Terminal) waitChild() error {
	awaitExit(t.cmd.Process.Pid)
	return t.cmd.Wait()
}

func (t *Terminal) forceChild() error {
	var err error
	if handleErr := t.cmd.Process.WithHandle(func(fd uintptr) {
		err = unix.PidfdSendSignal(int(fd), unix.SIGKILL, nil, 0)
	}); handleErr != nil {
		err = handleErr
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// The child is still unreaped, so its PID remains reserved while opening the
// pidfd. Park in Go's I/O poller before reaping to avoid a blocked OS thread
// per live terminal. When pidfds or polling are unavailable, cmd.Wait reaps
// the child directly.
func awaitExit(pid int) {
	fd, err := unix.PidfdOpen(pid, unix.PIDFD_NONBLOCK)
	if err != nil {
		return
	}
	file := os.NewFile(uintptr(fd), "terminal-pidfd")
	defer file.Close()
	raw, err := file.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Read(func(fd uintptr) bool {
		// Check readiness even on the first call, since the child may
		// already have exited before the runtime arms this read.
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(poll, 0)
			if err != unix.EINTR {
				return n != 0 || err != nil
			}
		}
	})
}

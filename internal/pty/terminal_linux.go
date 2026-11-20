package pty

import (
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// waitTerminalChild parks in Go's I/O poller until a started child exits, then reaps it.
// The caller must own the command's single Wait call.
func waitTerminalChild(cmd *exec.Cmd) error {
	awaitExit(cmd.Process.Pid)
	return cmd.Wait()
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

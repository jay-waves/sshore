package pty

import (
	"errors"
	"fmt"
	"os"

	"github.com/jay-waves/sshore/internal/sys"
	"golang.org/x/sys/unix"
)

type platformTerminal struct {
	cgroup *sys.TaskCgroup
}

func (t *Terminal) startChild(parent string) error {
	group, dir, err := sys.NewTaskCgroup(parent)
	if err != nil {
		return fmt.Errorf("create terminal cgroup: %w", err)
	}
	defer dir.Close()
	t.cmd.SysProcAttr.UseCgroupFD = true
	t.cmd.SysProcAttr.CgroupFD = int(dir.Fd())
	if err := t.cmd.Start(); err != nil {
		return errors.Join(fmt.Errorf("start shell in task cgroup: %w", err), group.Remove())
	}
	t.cgroup = group
	return nil
}

// waitTerminalChild parks in Go's I/O poller until a started child exits, then reaps it.
// The caller must own the command's single Wait call.
func (t *Terminal) waitChild() error {
	awaitExit(t.cmd.Process.Pid)
	return errors.Join(t.cmd.Wait(), t.cgroup.Kill(), t.cgroup.Remove())
}

func (t *Terminal) Stop() error { return t.cgroup.Kill() }

func (t *Terminal) Close() error { return errors.Join(t.Stop(), t.closeMaster()) }

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

package mux

import (
	"errors"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type Terminal struct {
	master   *os.File
	cmd      *exec.Cmd
	once     sync.Once
	closeErr error
	waitOnce sync.Once
	waitErr  error
}

// StartCommand starts a caller-configured command with a controlling terminal.
func StartCommand(cmd *exec.Cmd, size Size) (*Terminal, error) {
	if err := size.validate(); err != nil {
		return nil, err
	}
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
	if err != nil {
		return nil, err
	}
	// creack/pty uses Fd during setup, which marks its os.File as blocking.
	// Rewrap a nonblocking duplicate so Close and deadlines can interrupt I/O.
	fd := -1
	raw, e := master.SyscallConn()
	if e == nil {
		e = raw.Control(func(handle uintptr) { fd, err = unix.FcntlInt(handle, unix.F_DUPFD_CLOEXEC, 0) })
	}
	if e != nil {
		err = e
	}
	if err == nil {
		err = unix.SetNonblock(fd, true)
	}
	if err != nil {
		if fd >= 0 {
			unix.Close(fd)
		}
		master.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	master.Close()
	master = os.NewFile(uintptr(fd), "mux-pty")
	return &Terminal{master: master, cmd: cmd}, nil
}
func Start(c Config) (*Terminal, error) {
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = c.Dir
	return StartCommand(cmd, c.Size)
}
func (t *Terminal) Read(p []byte) (int, error) {
	n, err := t.master.Read(p)
	// Linux returns EIO after the last slave closes, including normal child exit.
	if errors.Is(err, syscall.EIO) {
		err = io.EOF
	}
	return n, err
}
func (t *Terminal) Write(p []byte) (int, error) { return t.master.Write(p) }
func (t *Terminal) Resize(s Size) error {
	if err := s.validate(); err != nil {
		return err
	}
	return t.control(func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: s.Cols, Row: s.Rows})
	})
}
func (t *Terminal) PID() int { return t.cmd.Process.Pid }

// Wait reaps the child once; concurrent callers receive the same result.
func (t *Terminal) Wait() error { t.waitOnce.Do(func() { t.waitErr = t.cmd.Wait() }); return t.waitErr }

// Close hangs up the terminal; deliberately detached descendants may survive.
func (t *Terminal) Close() error {
	t.once.Do(func() { t.closeErr = t.master.Close() })
	return t.closeErr
}

func (t *Terminal) SetWriteDeadline(at time.Time) error { return t.master.SetWriteDeadline(at) }
func (t *Terminal) control(fn func(int) error) error {
	raw, err := t.master.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err = raw.Control(func(fd uintptr) { opErr = fn(int(fd)) }); err != nil {
		return err
	}
	return opErr
}
func (t *Terminal) ForegroundGroup() (int, error) {
	var group int
	err := t.control(func(fd int) error { var e error; group, e = unix.IoctlGetInt(fd, unix.TIOCGPGRP); return e })
	return group, err
}
func (t *Terminal) winsize() (*unix.Winsize, error) {
	var size *unix.Winsize
	err := t.control(func(fd int) error { var e error; size, e = unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); return e })
	return size, err
}
func (t *Terminal) setWinsize(size *unix.Winsize) error {
	return t.control(func(fd int) error { return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, size) })
}

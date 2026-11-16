package tmux

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
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
	terminal := &Terminal{master: master, cmd: cmd}
	// A descendant may retain the slave after the shell exits. Reap the shell
	// independently, then bound output draining so it cannot keep the session alive.
	go func() {
		_ = terminal.Wait()
		_ = master.SetReadDeadline(time.Now().Add(time.Second))
	}()
	return terminal, nil
}
func Start(c Config) (*Terminal, error) {
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = c.Dir
	return StartCommand(cmd, c.Size)
}
func (t *Terminal) Read(p []byte) (int, error) {
	n, err := t.master.Read(p)
	// EIO marks the last slave closing; the read deadline ends post-exit draining.
	if errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrDeadlineExceeded) {
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

// The caller holds the session locks while output is enabled. Only nudge the
// shell's dimensions; foreground applications receive their usual SIGWINCH.
func RequestRedraw(master *Terminal, shellPID int) {
	group, err := master.ForegroundGroup()
	if err != nil || group <= 0 {
		return
	}
	if group == shellPID {
		size, e := master.winsize()
		if e == nil && size.Col > 1 {
			changed := *size
			changed.Col--
			if master.setWinsize(&changed) == nil {
				// Readline ignores SIGWINCH when the dimensions have not changed. Give
				// the shell a real resize, then restore it without injecting a keystroke.
				time.Sleep(30 * time.Millisecond)
				current, e := master.winsize()
				if e == nil && *current == changed {
					_ = master.setWinsize(size)
				}
				return
			}
		}
	}
	_ = unix.Kill(-group, unix.SIGWINCH)
}

func NewTerminalInput(t *Terminal) (*InputLease, error) { return NewInputLease(t), nil }

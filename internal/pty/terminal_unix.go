//go:build linux || (darwin && arm64)

package pty

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	nativepty "github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type Terminal struct {
	master      *os.File
	cmd         *exec.Cmd
	closeMaster func() error
	wait        func() error
}

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

// Start owns the command and its controlling terminal from creation to Wait.
func Start(c Config) (result *Terminal, _ error) {
	if err := c.Size.validate(); err != nil {
		return nil, err
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env, cmd.Dir = c.Env, c.Dir
	master, slave, err := openTerminalPair()
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			_ = master.Close()
		}
	}()
	terminal := &Terminal{master: master, cmd: cmd, closeMaster: sync.OnceValue(master.Close)}
	defer slave.Close()
	if err := terminal.setWinsize(&unix.Winsize{Col: c.Size.Cols, Row: c.Size.Rows}); err != nil {
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	cmd.Env = append(cmd.Environ(), "SSHORE_TTY="+slave.Name())
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// A descendant may retain the slave after the shell exits. Reap the shell
	// independently, then bound output draining so it cannot keep the session alive.
	terminal.wait = sync.OnceValue(func() error {
		err := waitTerminalChild(cmd)
		_ = master.SetReadDeadline(time.Now().Add(time.Second))
		return err
	})
	go terminal.Wait()
	return terminal, nil
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
	return t.setWinsize(&unix.Winsize{Col: s.Cols, Row: s.Rows})
}
func (t *Terminal) PID() int { return t.cmd.Process.Pid }

// Wait reaps the child once; concurrent callers receive the same result.
func (t *Terminal) Wait() error { return t.wait() }

// Close hangs up the terminal; deliberately detached descendants may survive.
func (t *Terminal) Close() error { return t.closeMaster() }

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

// The caller holds the session locks while output is enabled. A real size
// change prompts applications that ignore a SIGWINCH with unchanged dimensions.
func RequestRedraw(master *Terminal) {
	group, err := master.ForegroundGroup()
	if err != nil || group <= 0 {
		return
	}
	{
		size, e := master.winsize()
		if e == nil && size.Col > 1 {
			changed := *size
			changed.Col--
			if master.setWinsize(&changed) == nil {
				// Restore the original size without overwriting a concurrent resize.
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

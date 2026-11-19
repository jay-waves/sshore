package tmux

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Terminal struct {
	master      *os.File
	cmd         *exec.Cmd
	closeMaster func() error
	wait        func() error
}

// Start owns the command and its controlling terminal from creation to Wait.
func Start(c Config) (result *Terminal, _ error) {
	if err := c.Size.validate(); err != nil {
		return nil, err
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env, cmd.Dir = c.Env, c.Dir
	// Keep the master pollable from startup so deadlines can interrupt I/O.
	// SyscallConn lets us configure it without changing its blocking mode.
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			_ = master.Close()
		}
	}()
	terminal := &Terminal{master: master, cmd: cmd, closeMaster: sync.OnceValue(master.Close)}
	var number int
	if err := terminal.control(func(fd int) error {
		var err error
		number, err = unix.IoctlGetInt(fd, unix.TIOCGPTN)
		if err != nil {
			return err
		}
		return unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0)
	}); err != nil {
		return nil, err
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
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
		err := cmd.Wait()
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

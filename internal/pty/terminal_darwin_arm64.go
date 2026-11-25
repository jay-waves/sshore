package pty

import (
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type platformTerminal struct {
	stop func() error
}

func (t *Terminal) startChild(_ string) error {
	t.stop = sync.OnceValue(func() error { return stopTerminal(t.exited, t.Close, t.forceChild) })
	return t.cmd.Start()
}

func (t *Terminal) Stop() error { return t.stop() }

func (t *Terminal) Close() error { return t.closeMaster() }

func (t *Terminal) waitChild() error {
	fd, err := unix.Kqueue()
	if err == nil {
		unix.CloseOnExec(fd)
		defer unix.Close(fd)
		event := unix.Kevent_t{Ident: uint64(t.cmd.Process.Pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
		if _, err = unix.Kevent(fd, []unix.Kevent_t{event}, nil, nil); err == nil {
			// The PID stays reserved until Wait reaps the child. Disable
			// signaling under the same lock before entering Wait.
			t.childMu.Lock()
			t.signalSafe = true
			t.childMu.Unlock()
			var events [1]unix.Kevent_t
			for {
				if _, err = unix.Kevent(fd, nil, events[:], nil); err != unix.EINTR {
					break
				}
			}
			t.childMu.Lock()
			t.signalSafe = false
			t.childMu.Unlock()
		}
	}
	return t.cmd.Wait()
}

func (t *Terminal) forceChild() error {
	t.childMu.Lock()
	defer t.childMu.Unlock()
	if !t.signalSafe {
		return errors.New("cannot safely signal the shell")
	}
	if err := t.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

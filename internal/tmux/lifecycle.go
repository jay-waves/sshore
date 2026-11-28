package tmux

import (
	"errors"
	"time"
)

// Events connects terminal transitions to the application. These callbacks are
// called with the task guard released. Exit runs before streams are closed.
type Events struct {
	Changed     func(...string)
	Exit        func(*Attachment, Exit)
	ParentEnded func(string, bool)
	StopFailed  func(error)
	Closed      func()
}

type Exit struct {
	Code   int
	Err    error
	Reason string
}

func (t *Session) changed() {
	if t.events.Changed != nil {
		var ids []string
		for node := t.node; node != nil; node = node.Parent() {
			ids = append(ids, node.id)
		}
		t.events.Changed(ids...)
	}
}

// expireUnstarted closes a session that has no terminal when the timer fires.
func (t *Session) expireUnstarted(after time.Duration) {
	t.guard.Lock()
	defer t.guard.Unlock()
	if !t.startedLocked() && !t.exited() {
		t.setTimer(after, func() {
			t.guard.Lock()
			if t.startedLocked() {
				t.guard.Unlock()
				return
			}
			owner, ok := t.beginCloseLocked()
			t.guard.Unlock()
			if ok {
				t.completeClose(owner)
			}
		})
	}
}

// Close withdraws the task from the tree and closes its streams and terminal.
func (t *Session) Close() {
	t.guard.Lock()
	owner, ok := t.beginCloseLocked()
	t.guard.Unlock()
	if ok {
		t.completeClose(owner)
	}
}

// Only the caller that withdraws the task may publish its completion.
func (t *Session) beginCloseLocked() (owner *Attachment, ok bool) {
	if t.closing {
		return owner, false
	}
	t.closing = true
	return t.endLocked(), true
}

func (t *Session) completeClose(owner *Attachment) {
	t.cancelPending()
	if owner != nil {
		owner.Close()
	}
	if t.terminal != nil {
		_ = t.terminal.Close()
	}
	t.manager.remove(t)
	if t.events.Closed != nil {
		t.events.Closed()
	}
	close(t.done)
	t.changed()
}

// Stop requests process termination and waits for the terminal to close.
func (t *Session) Stop(reason string) error {
	t.guard.Lock()
	if t.exited() || t.stoppingLocked() {
		t.guard.Unlock()
		return t.waitForClose(5*time.Second, "shell has not finished stopping")
	}
	if reason == "" {
		reason = "shell stop requested"
	}
	t.stopReason = reason
	t.guard.Unlock()
	return t.stopTerminal()
}

func (t *Session) cancelPending() {
	t.guard.Lock()
	pending := t.pending
	t.guard.Unlock()
	if pending != nil {
		pending.Close()
	}
}

func (t *Session) stopTerminal() error {
	t.cancelPending()
	t.guard.Lock()
	terminal := t.terminal
	t.guard.Unlock()
	if terminal == nil {
		t.Close()
		return nil
	}
	if err := terminal.Stop(); err != nil {
		t.guard.Lock()
		if !t.exited() {
			t.stopReason = ""
		}
		t.guard.Unlock()
		t.changed()
		return err
	}
	t.changed()
	return t.waitForClose(10*time.Second, "shell process ended, but terminal cleanup timed out")
}

func (t *Session) waitForClose(timeout time.Duration, detail string) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.done:
		return nil
	case <-timer.C:
		return errors.New(detail)
	}
}

func (t *Session) terminalExited(err error) {
	t.guard.Lock()
	owner, ok := t.beginCloseLocked()
	exit := Exit{Err: err, Reason: t.stopReason}
	t.guard.Unlock()
	if !ok {
		return
	}
	defer t.completeClose(owner)
	if err != nil {
		exit.Code = 1
		if e, ok := errors.AsType[interface {
			error
			ExitCode() int
		}](err); ok {
			exit.Code = e.ExitCode()
		}
	}
	t.changed()
	if t.events.Exit != nil {
		t.events.Exit(owner, exit)
	}
}

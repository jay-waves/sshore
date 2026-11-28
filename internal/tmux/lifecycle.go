package tmux

import (
	"errors"
	"time"
)

// Events connects terminal transitions to the application. These callbacks are
// called with the task guard released. Exit runs before streams are closed.
type Events[A attachmentOwner, T any] struct {
	Changed        func(...string)
	Exit           func(A, Exit)
	ParentEnded    func(T, bool)
	StopFailed     func(error)
	AttachmentLost func(controlErr, releaseErr error)
}

type Exit struct {
	Code   int
	Err    error
	Reason string
}

func (t *Task[A, T]) changed() {
	if t.events.Changed != nil {
		var ids []string
		for node := t.node; node != nil; node = node.Parent() {
			ids = append(ids, node.id)
		}
		t.events.Changed(ids...)
	}
}

// ExpireUnstarted closes a task that has no terminal when the timer fires.
func (t *Task[A, T]) ExpireUnstarted(after time.Duration) {
	t.guard.Lock()
	defer t.guard.Unlock()
	if !t.Started() && !t.Exited() {
		t.setTimer(after, func() {
			t.guard.Lock()
			if t.Started() {
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
func (t *Task[A, T]) Close() {
	t.guard.Lock()
	owner, ok := t.beginCloseLocked()
	t.guard.Unlock()
	if ok {
		t.completeClose(owner)
	}
}

// Only the caller that withdraws the task may publish its completion.
func (t *Task[A, T]) beginCloseLocked() (owner A, ok bool) {
	if t.closing {
		return owner, false
	}
	t.closing = true
	return t.endLocked(), true
}

func (t *Task[A, T]) completeClose(owner A) {
	t.closePendingTerminals()
	var empty A
	if owner != empty {
		owner.stream().Close()
	}
	if t.terminal != nil {
		_ = t.terminal.Close()
	}
	t.manager.complete(t)
	t.changed()
}

// Stop requests process termination and waits for the terminal to close.
func (t *Task[A, T]) Stop(reason string) error {
	t.guard.Lock()
	if t.Exited() || t.Stopping() {
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

func (t *Task[A, T]) stopTerminal() error {
	t.guard.Lock()
	terminal := t.terminal
	t.guard.Unlock()
	if terminal == nil {
		t.Close()
		return nil
	}
	if err := terminal.Stop(); err != nil {
		t.guard.Lock()
		if !t.Exited() {
			t.stopReason = ""
		}
		t.guard.Unlock()
		t.changed()
		return err
	}
	t.changed()
	return t.waitForClose(10*time.Second, "shell process ended, but terminal cleanup timed out")
}

func (t *Task[A, T]) waitForClose(timeout time.Duration, detail string) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.done:
		return nil
	case <-timer.C:
		return errors.New(detail)
	}
}

func (t *Task[A, T]) terminalExited(err error) {
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

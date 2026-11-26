package tmux

import (
	"errors"
	"time"
)

func (t *Task[A, T]) PinState() (bool, time.Time) {
	if t.pinUntil == nil {
		return false, time.Time{}
	}
	return true, *t.pinUntil
}

// expire acquires the task lock and rechecks the current retention policy.
func (t *Task[A, T]) Pin(ttl time.Duration, stopping bool, expire func()) error {
	if ttl < 0 {
		return errors.New("invalid pin duration")
	}
	if t.Exited() || stopping || !t.Started() {
		return errors.New("shell is ending or has not started")
	}
	var deadline time.Time
	if ttl != 0 {
		deadline = time.Now().Add(ttl)
	}
	t.pinUntil = &deadline
	t.setTimer(ttl, expire)
	return nil
}

// Unretained reports when the owner has gone and no pin keeps the task alive.
// An active attachment can finish even if its last pin has expired.
func (t *Task[A, T]) Unretained(now time.Time) bool {
	if parent := t.node.Parent(); parent != nil && parent.Ended() {
		t.scopeLost = true
	}
	var empty A
	if t.pinUntil != nil {
		if t.pinUntil.IsZero() || now.Before(*t.pinUntil) || t.scopeLost && t.active != empty {
			return false
		}
		t.pinUntil = nil
		t.stopTimer()
	}
	return t.scopeLost
}

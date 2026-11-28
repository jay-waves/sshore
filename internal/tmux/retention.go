package tmux

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"time"
)

func (t *Task[A, T]) pinStateLocked() (bool, time.Time) {
	if t.pinUntil == nil {
		return false, time.Time{}
	}
	return true, *t.pinUntil
}

// RequestDetach pins the active session before asking its frontend to detach.
func (t *Task[A, T]) RequestDetach(ttl time.Duration) (A, time.Time, error) {
	t.guard.Lock()
	var empty A
	active := t.active
	if active == empty {
		t.guard.Unlock()
		return empty, time.Time{}, errors.New("shell has no active attachment")
	}
	err := t.pinLocked(ttl)
	_, until := t.pinStateLocked()
	t.guard.Unlock()
	if err == nil {
		t.changed()
	}
	return active, until, err
}

func (t *Task[A, T]) pinLocked(ttl time.Duration) error {
	if ttl < 0 {
		return errors.New("invalid pin duration")
	}
	if t.Exited() || t.Stopping() || !t.Started() {
		return errors.New("shell is ending or has not started")
	}
	var deadline time.Time
	if ttl != 0 {
		deadline = time.Now().Add(ttl)
	}
	t.pinUntil = &deadline
	t.setTimer(ttl, func() {
		t.guard.Lock()
		t.reapUnretainedLocked()
		t.guard.Unlock()
		t.changed()
	})
	return nil
}

// unretained reports when the owner has gone and no pin keeps the task alive.
// An active attachment can finish even if its last pin has expired.
func (t *Task[A, T]) unretained(now time.Time) bool {
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

func (t *Task[A, T]) reapUnretainedLocked() {
	if t.Exited() || t.Stopping() || !t.unretained(time.Now()) {
		return
	}
	t.stopReason = "shell lifetime ended"
	go func() {
		if err := t.stopTerminal(); err != nil && t.events.StopFailed != nil {
			t.events.StopFailed(err)
		}
	}()
}

func (t *Task[A, T]) watchParent(parent *treeNode[T]) {
	go func() {
		select {
		case <-parent.done:
			live, stopping := t.endScope(parent)
			if live && t.events.ParentEnded != nil {
				t.events.ParentEnded(parent.Value, stopping)
			}
		case <-t.done:
		}
	}()
}

// ClaimOwner binds the root's starting connection to this session. SSH has
// already authenticated the connection and validated the parent and token.
func (t *Task[A, T]) ClaimOwner(parent, token string) bool {
	t.guard.Lock()
	if t.ownerClaimed || t.node.Parent() != nil || t.Exited() || t.Stopping() ||
		parent != "" && t.Started() {
		t.guard.Unlock()
		return false
	}
	t.ownerClaimed = true
	t.remoteOwner, t.remoteOwnerToken = parent, token
	t.guard.Unlock()
	t.changed()
	return true
}

func (t *Task[A, T]) HasRemoteOwner(token string) bool {
	t.guard.Lock()
	defer t.guard.Unlock()
	return t.remoteOwner != "" && t.checkOwnerLocked(token) == nil
}

func (t *Task[A, T]) checkOwnerLocked(token string) error {
	if subtle.ConstantTimeCompare([]byte(token), []byte(t.remoteOwnerToken)) == 1 {
		return nil
	}
	if parent := t.remoteOwner; parent != "" {
		return fmt.Errorf("enter shell %s from its parent %s", t.node.Label, parent[:min(6, len(parent))])
	}
	return errors.New("parent shell route ended")
}

// OwnerEnded revokes the route and applies the session's retention policy.
func (t *Task[A, T]) OwnerEnded() (live, stopping bool) { return t.endScope(nil) }

func (t *Task[A, T]) endScope(parent *treeNode[T]) (live, stopping bool) {
	t.guard.Lock()
	if t.node.Parent() != parent {
		t.guard.Unlock()
		return false, false
	}
	live = !t.Exited() && !t.Stopping()
	if parent != nil {
		t.manager.forest.setParent(t.node, nil)
	} else {
		t.remoteOwner, t.remoteOwnerToken = "", ""
	}
	t.scopeLost = true
	t.reapUnretainedLocked()
	stopping = t.Stopping()
	t.guard.Unlock()
	t.changed()
	return live, stopping
}

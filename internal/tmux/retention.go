package tmux

import (
	"errors"

	"time"
)

func (t *Session) pinStateLocked() (bool, time.Time) {
	if t.pinUntil == nil {
		return false, time.Time{}
	}
	return true, *t.pinUntil
}

// RequestDetach pins the active session before asking its frontend to detach.
func (t *Session) RequestDetach(ttl time.Duration) (*Attachment, time.Time, error) {
	t.guard.Lock()
	active := t.active
	if active == nil {
		t.guard.Unlock()
		return nil, time.Time{}, errors.New("shell has no active attachment")
	}
	err := t.pinLocked(ttl)
	_, until := t.pinStateLocked()
	t.guard.Unlock()
	if err == nil {
		t.changed()
	}
	return active, until, err
}

func (t *Session) pinLocked(ttl time.Duration) error {
	if ttl < 0 {
		return errors.New("invalid pin duration")
	}
	if t.exited() || t.stoppingLocked() || !t.startedLocked() {
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
func (t *Session) unretained(now time.Time) bool {
	if parent := t.node.Parent(); parent != nil && parent.Ended() {
		t.scopeLost = true
	}
	if t.pinUntil != nil {
		if t.pinUntil.IsZero() || now.Before(*t.pinUntil) || t.scopeLost && t.active != nil {
			return false
		}
		t.pinUntil = nil
		t.stopTimer()
	}
	return t.scopeLost
}

func (t *Session) reapUnretainedLocked() {
	if t.exited() || t.stoppingLocked() || !t.unretained(time.Now()) {
		return
	}
	t.stopReason = "shell lifetime ended"
	go func() {
		if err := t.stopTerminal(); err != nil && t.events.StopFailed != nil {
			t.events.StopFailed(err)
		}
	}()
}

func (t *Session) watchParent(parent *treeNode) {
	go func() {
		select {
		case <-parent.done:
			live, stopping := t.endScope(parent)
			if live && t.events.ParentEnded != nil {
				t.events.ParentEnded(parent.id, stopping)
			}
		case <-t.done:
		}
	}()
}

// ClaimOwner binds an authenticated owner's lifetime to this session. The
// caller validates the owner and signals OwnerEnded when its lease ends.
func (t *Session) ClaimOwner() bool {
	t.guard.Lock()
	if t.ownerClaimed || t.node.Parent() != nil || t.exited() || t.stoppingLocked() {
		t.guard.Unlock()
		return false
	}
	t.ownerClaimed = true
	t.guard.Unlock()
	t.changed()
	return true
}

// OwnerEnded revokes the route and applies the session's retention policy.
func (t *Session) OwnerEnded() (live, stopping bool) { return t.endScope(nil) }

func (t *Session) endScope(parent *treeNode) (live, stopping bool) {
	t.guard.Lock()
	if t.node.Parent() != parent {
		t.guard.Unlock()
		return false, false
	}
	live = !t.exited() && !t.stoppingLocked()
	if parent != nil {
		t.manager.forest.setParent(t.node, nil)

	}
	t.scopeLost = true
	t.reapUnretainedLocked()
	stopping = t.stoppingLocked()
	t.guard.Unlock()
	t.changed()
	return live, stopping
}

package main

import (
	"cmp"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"slices"
	"time"
)

func parseDetachCommand(args []string) (message, error) {
	m := message{Type: "detach"}
	parsed, err := parseCLI(args, "detach")
	if err != nil {
		return m, err
	}
	if v := parsed.operand(0); v != "" {
		m.TTL, err = time.ParseDuration(v)
		if err != nil || m.TTL <= 0 {
			return m, usage("invalid duration %q; use a positive duration such as 30s, 60m or 2h", v)
		}
	}
	return m, nil
}

func requestShellRetention(c io.ReadWriter, request message) error {
	request.SoftwareVersion = version
	if err := writeMessage(c, request); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return err
	}
	if reply.Type != "pinned" {
		return fmt.Errorf("%s: %s", request.Type, reply.Error)
	}
	return nil
}

// A root lease belongs to the frontend. A child on another daemon gets its
// parent identity and private entry token from this lease.
func (s *shellTask) serveOwner(c net.Conn, m message) error {
	s.mu.Lock()
	if s.ownerClaimed || s.parent != nil || s.exited || s.killing || m.Instance != s.record.UID || m.SoftwareVersion != version ||
		m.Owner == "" && m.Token != "" || m.Owner != "" && (!s.hosted || !validTaskUID(m.Owner) || len(m.Token) != 64 || s.terminal != nil) {
		s.mu.Unlock()
		return writeMessage(c, message{Type: "error", Error: "shell owner unavailable"})
	}
	s.ownerClaimed = true
	s.remoteOwner, s.remoteOwnerToken = m.Owner, m.Token
	s.mu.Unlock()
	defer s.scopeEnded(nil)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-s.done:
			c.Close()
		case <-done:
		}
	}()
	if activity := accountActivity.Load(); activity != nil {
		activity.serve(c, leaseRequest{Kind: "heartbeat"})
		return nil
	}
	return fmt.Errorf("account daemon unavailable")
}

type ownedShellConnection struct {
	net.Conn
	token string
}

func (s *shellTask) checkRemoteOwnerLocked(c attachmentControl) error {
	var token string
	if owned, ok := c.(*ownedShellConnection); ok {
		token = owned.token
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.remoteOwnerToken)) == 1 {
		return nil
	}
	if s.remoteOwner != "" {
		return fmt.Errorf("enter shell %s from its parent %s", taskShortCode(s.treeIdentity()), taskShortCode(identityFromUID(s.remoteOwner)))
	}
	return fmt.Errorf("parent shell route ended")
}

func (s *shellTask) watchParent(parent *shellTask) {
	if parent == nil {
		return
	}
	go func() {
		select {
		case <-parent.done:
			s.scopeEnded(parent)
		case <-s.done:
		}
	}()
}

func (s *shellTask) scopeEnded(owner *shellTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parent != owner {
		return
	}
	if owner == nil {
		s.remoteOwner, s.remoteOwnerToken = "", ""
	} else {
		// Pinned survivors no longer need the terminated parent's task or routes.
		s.tasks.forest.mu.Lock()
		s.parent = nil
		s.tasks.forest.mu.Unlock()
	}
	s.scopeLost = true
	s.reapUnretainedLocked()
}

func (s *shellTask) pinLocked(ttl time.Duration) error {
	if ttl < 0 {
		return fmt.Errorf("invalid pin duration")
	}
	if s.exited || s.killing || s.terminal == nil {
		return fmt.Errorf("shell is ending or has not started")
	}
	if s.pinTimer != nil {
		s.pinTimer.Stop()
		s.pinTimer = nil
	}
	s.pinned = true
	s.pinUntil = time.Time{}
	if ttl != 0 {
		s.pinUntil = time.Now().Add(ttl)
		deadline := s.pinUntil
		s.pinTimer = time.AfterFunc(ttl, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.pinUntil == deadline {
				s.reapUnretainedLocked()
			}
		})
	}
	return nil
}

// At expiry an attached shell can finish its current attachment. A later
// attachment must still have a live parent or an unexpired pin.
func (s *shellTask) reapUnretainedLocked() {
	if s.exited || s.killing {
		return
	}
	if s.parent != nil && s.parent.treeEnded.Load() {
		s.scopeLost = true
	}
	if s.pinned {
		if s.pinUntil.IsZero() || time.Now().Before(s.pinUntil) || s.scopeLost && s.active != nil {
			return
		}
		s.pinned = false
		s.pinUntil = time.Time{}
	}
	if s.scopeLost {
		s.killing, s.exitReason = true, "shell lifetime ended"
		go func() { _ = s.stopTerminal() }()
	}
}

func (s *shellTask) parentIdentityLocked() string {
	if s.parent != nil && !s.parent.treeEnded.Load() {
		return s.parent.treeIdentity()
	}
	return identityFromUID(s.remoteOwner)
}

// The owner lease is still live while the exit frame is being assembled.
func (s *shellTask) retainedChildren() []retainedShell {
	type managedTask interface {
		daemonTask
		managedShell() *shellTask
	}
	var retained []retainedShell
	now := time.Now()
	for _, task := range liveTasks[managedTask](s.tasks) {
		child := task.managedShell()
		child.mu.Lock()
		if child.parent == s && !child.exited && !child.killing && child.pinned &&
			(child.pinUntil.IsZero() || now.Before(child.pinUntil)) {
			retained = append(retained, retainedShell{Instance: child.record.UID, Name: child.record.Name, PinUntil: child.pinUntil})
		}
		child.mu.Unlock()
	}

	s.mu.Lock()
	remote := make([]*remoteShellChild, 0, len(s.remoteChildren))
	for _, child := range s.remoteChildren {
		remote = append(remote, child)
	}
	s.mu.Unlock()
	if len(remote) > 0 {
		replies := make(chan retainedShell, len(remote))
		for _, child := range remote {
			go func() {
				status, err := child.status()
				if err != nil || status.Owner != s.treeIdentity() || status.Action != taskDetached ||
					!status.Pinned || !status.PinUntil.IsZero() && !time.Now().Before(status.PinUntil) {
					replies <- retainedShell{}
					return
				}
				replies <- retainedShell{Host: child.host, Instance: child.uid, Name: status.Name, PinUntil: status.PinUntil}
			}()
		}
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
	collect:
		for range remote {
			select {
			case child := <-replies:
				if child.Instance != "" {
					retained = append(retained, child)
				}
			case <-timer.C:
				break collect
			}
		}
	}
	slices.SortFunc(retained, func(a, b retainedShell) int {
		return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Instance, b.Instance))
	})
	return retained[:min(len(retained), 64)]
}

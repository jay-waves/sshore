package main

import (
	"fmt"
	"io"
	"net"
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

// The root lease belongs to the frontend. Nested shells on this daemon
// follow the registered parent task instead of a PID or another SSH lease.
func (s *shellTask) serveOwner(c net.Conn, m message) error {
	s.mu.Lock()
	if s.ownerClaimed || s.parent != nil || s.exited || s.killing || m.Instance != s.record.UID || m.SoftwareVersion != version {
		s.mu.Unlock()
		return writeMessage(c, message{Type: "error", Error: "shell owner unavailable"})
	}
	s.ownerClaimed = true
	s.mu.Unlock()
	defer s.scopeEnded()
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

func (s *shellTask) watchParent() {
	if s.parent == nil {
		return
	}
	go func() {
		select {
		case <-s.parent.done:
			s.scopeEnded()
		case <-s.done:
		}
	}()
}

func (s *shellTask) scopeEnded() {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if s.parent == nil || s.parent.expired() {
		return ""
	}
	return s.parent.treeIdentity()
}

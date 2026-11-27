package main

import (
	"cmp"
	"crypto/subtle"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"time"

	"github.com/jay-waves/sshore/internal/termui"
)

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
func (s *tmuxTask) serveOwner(c net.Conn, m message) (result error) {
	s.mu.Lock()
	if s.ownerClaimed || s.parentTask() != nil || s.runtime.Exited() || s.killing || m.Instance != s.record.UID || m.SoftwareVersion != version ||
		m.Owner == "" && m.Token != "" || m.Owner != "" && (!s.hosted || !validTaskUID(m.Owner) || len(m.Token) != 64 || s.runtime.Started()) {
		s.mu.Unlock()
		return writeMessage(c, message{Type: "error", Error: "shell owner unavailable"})
	}
	s.ownerClaimed = true
	s.remoteOwner, s.remoteOwnerToken = m.Owner, m.Token
	s.mu.Unlock()
	s.registry.inventoryChanged(s)
	defer func() { s.scopeEnded(nil, result) }()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-s.runtime.Done():
			c.Close()
		case <-done:
		}
	}()
	if activity := accountActivity.Load(); activity != nil {
		return activity.serve(c, leaseRequest{Kind: "heartbeat"}, s.hosted)
	}
	return fmt.Errorf("account daemon unavailable")
}

type ownedShellConnection struct {
	net.Conn
	token string
}

func (s *tmuxTask) checkRemoteOwnerLocked(c attachmentControl) error {
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

func (s *tmuxTask) parentTask() *tmuxTask {
	parent, _ := s.runtime.Parent()
	return parent
}

func (s *tmuxTask) watchParent(parent *tmuxTask) {
	if parent == nil {
		return
	}
	go func() {
		select {
		case <-parent.runtime.Done():
			s.scopeEnded(parent, nil)
		case <-s.runtime.Done():
		}
	}()
}

func (s *tmuxTask) scopeEnded(owner *tmuxTask, cause error) {
	s.mu.Lock()
	if s.parentTask() != owner {
		s.mu.Unlock()
		return
	}
	reason := "Shell owner released its lease"
	level := termui.Info
	if owner == nil {
		s.remoteOwner, s.remoteOwnerToken = "", ""
		if cause == io.EOF {
			reason = "Shell owner connection closed"
		} else if cause != nil {
			level = termui.Error
			reason = "Shell owner connection ended: " + cleanText(cause.Error())
		}
	} else {
		reason = "Parent shell " + canonicalTaskCode(owner.treeIdentity()) + " ended"
	}
	ending := s.runtime.Exited() || s.killing
	s.runtime.LoseScope()
	s.reapUnretainedLocked()
	reaping := s.killing
	s.mu.Unlock()
	s.registry.inventoryChanged(s)
	if !ending {
		outcome := "Keeping pinned shell."
		if reaping {
			outcome = "Stopping unpinned shell."
		}
		termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), level, "%s. %s", reason, outcome)
	}
}

func (s *tmuxTask) pinLocked(ttl time.Duration) error {
	return s.runtime.Pin(ttl, s.killing, func() {
		s.mu.Lock()
		s.reapUnretainedLocked()
		s.mu.Unlock()
		s.registry.inventoryChanged(s)
	})
}

func (s *tmuxTask) reapUnretainedLocked() {
	if s.runtime.Exited() || s.killing || !s.runtime.Unretained(time.Now()) {
		return
	}
	s.killing, s.exitReason = true, "shell lifetime ended"
	go func() {
		if err := s.stopTerminal(); err != nil {
			termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Could not stop unpinned shell: %s", cleanText(err.Error()))
		}
	}()
}

func (s *tmuxTask) parentIdentityLocked() string {
	if parent, ended := s.runtime.Parent(); parent != nil && !ended {
		return parent.treeIdentity()
	}
	return identityFromUID(s.remoteOwner)
}

// The owner lease is still live while the exit frame is being assembled.
func (s *tmuxTask) retainedChildren() []retainedShell {
	var retained []retainedShell
	now := time.Now()
	for _, child := range s.registry.childShells(s) {
		child.mu.Lock()
		pinned, until := child.runtime.PinState()
		if child.parentTask() == s && !child.runtime.Exited() && !child.killing && pinned && (until.IsZero() || now.Before(until)) {
			retained = append(retained, retainedShell{Instance: child.record.UID, Name: child.record.Name, PinUntil: until})
		}
		child.mu.Unlock()
	}

	s.mu.Lock()
	remote := slices.Collect(maps.Keys(s.remoteChildren))
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

package main

import (
	"cmp"
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
	if m.Instance != s.record.UID || m.SoftwareVersion != version ||
		m.Owner == "" && m.Token != "" || m.Owner != "" && (!s.hosted || !validTaskUID(m.Owner) || len(m.Token) != 64) ||
		!s.runtime.ClaimOwner(m.Owner, m.Token) {
		return writeMessage(c, message{Type: "error", Error: "shell owner unavailable"})
	}
	defer func() { s.ownerConnectionEnded(result) }()
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

func (s *tmuxTask) ownerConnectionEnded(cause error) {
	live, stopping := s.runtime.OwnerEnded()
	if !live {
		return
	}
	reason := "Shell owner released its lease"
	level := termui.Info
	if cause == io.EOF {
		reason = "Shell owner connection closed"
	} else if cause != nil {
		level = termui.Error
		reason = "Shell owner connection ended: " + cleanText(cause.Error())
	}
	s.reportScopeEnd(level, reason, stopping)
}

func (s *tmuxTask) reportParentEnd(parent *tmuxTask, stopping bool) {
	s.reportScopeEnd(termui.Info, "Parent shell "+canonicalTaskCode(parent.treeIdentity())+" ended", stopping)
}

func (s *tmuxTask) reportScopeEnd(level termui.Color, reason string, stopping bool) {
	outcome := "Keeping pinned shell."
	if stopping {
		outcome = "Stopping unpinned shell."
	}
	termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), level, "%s. %s", reason, outcome)
}

func (s *tmuxTask) reportReapFailure(err error) {
	termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Could not stop unpinned shell: %s", cleanText(err.Error()))
}

// The owner lease is still live while the exit frame is being assembled.
func (s *tmuxTask) retainedChildren() []retainedShell {
	var retained []retainedShell
	now := time.Now()
	for _, child := range s.registry.shells.Children(s.runtime) {
		view := child.runtime.Snapshot()
		if view.ParentID == s.record.UID && view.State != taskExited && !view.Stopping && view.Pinned && (view.PinUntil.IsZero() || now.Before(view.PinUntil)) {
			retained = append(retained, retainedShell{Instance: view.ID, Name: view.Name, PinUntil: view.PinUntil})
		}
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

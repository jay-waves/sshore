package main

import (
	"cmp"
	"fmt"
	"io"

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
func (s *shellTask) serveOwner(c net.Conn, m message) (result error) {
	if m.Instance != s.record.UID || m.SoftwareVersion != version ||
		m.Owner != "" || m.Token != "" ||
		!s.session.ClaimOwner() {
		return writeMessage(c, message{Type: "error", Error: "shell owner unavailable"})
	}
	defer func() { s.ownerConnectionEnded(result) }()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-s.session.Done():
			c.Close()
		case <-done:
		}
	}()
	if activity := accountActivity.Load(); activity != nil {
		return activity.serve(c, leaseRequest{Kind: "heartbeat"}, s.hosted)
	}
	return fmt.Errorf("account daemon unavailable")
}

func (s *shellTask) ownerConnectionEnded(cause error) {
	live, stopping := s.session.OwnerEnded()
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

func (s *shellTask) reportParentEnd(parent string, stopping bool) {
	s.reportScopeEnd(termui.Info, "Parent shell "+canonicalTaskCode(taskKey(parent))+" ended", stopping)
}

func (s *shellTask) reportScopeEnd(level termui.Color, reason string, stopping bool) {
	outcome := "Keeping pinned shell."
	if stopping {
		outcome = "Stopping unpinned shell."
	}
	termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), level, "%s. %s", reason, outcome)
}

func (s *shellTask) reportReapFailure(err error) {
	termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Could not stop unpinned shell: %s", cleanText(err.Error()))
}

// The owner lease is still live while the exit frame is being assembled.
func (s *shellTask) retainedChildren() []retainedShell {
	var retained []retainedShell
	now := time.Now()
	for _, child := range s.registry.sessions.Children(s.session) {
		view := child.Snapshot()
		if view.ParentID == s.record.UID && view.State != taskExited && !view.Stopping && view.Pinned && (view.PinUntil.IsZero() || now.Before(view.PinUntil)) {
			retained = append(retained, retainedShell{Instance: view.ID, Name: view.Name, PinUntil: view.PinUntil})
		}
	}

	slices.SortFunc(retained, func(a, b retainedShell) int {
		return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Instance, b.Instance))
	})
	return retained[:min(len(retained), 64)]
}

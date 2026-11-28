package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/tmux"
)

// attachment binds the application's control messages to a tmux byte stream.
type attachment struct {
	*tmux.Attachment
	sourceHost string
	control    attachmentControl
	controlMu  sync.Mutex
	approvals  approvalExchanges
}

func (a *attachment) controlMessage(m message) error {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	return streamMessage(a.control, m)
}

func (a *attachment) childResult(err error) {
	if err != nil {
		_ = a.controlMessage(message{Type: "sh_child_error", Error: err.Error()})
	}
}

func (a *attachment) finishExit(exit message) {
	exit.TerminalOffset, exit.OutputIncomplete = a.OutputStatus()
	_ = a.controlMessage(exit)
	select {
	case <-a.Done():
	case <-time.After(2 * time.Second):
		a.Close()
	}
}

func expectDetachMessage(r io.Reader, expected string) (message, error) {
	m, err := readMessage(r)
	if err != nil {
		return m, err
	}
	if m.Type != expected {
		return m, fmt.Errorf("expected %s in detach handshake, received %s", expected, m.Type)
	}
	return m, nil
}

// FinishDetach confirms the handoff after the host releases the input lease.
// Only the control loop may call it, since it consumes the final acknowledgement.
func (a *attachment) finishDetach(reply message) error {
	if err := a.controlMessage(reply); err != nil {
		return err
	}
	_ = a.control.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := expectDetachMessage(a.control, "bye")
	return err
}

func (s *shellTask) attach(c net.Conn, m message, extra func(*attachment, message) error) (result error) {
	// The initial request deadline must not expire an attached shell.
	_ = c.SetReadDeadline(time.Time{})
	p := &attachment{control: c}
	terminal := s.takeTerminal(m.Token)
	if terminal == nil {
		return s.attachRejected(p, errTerminalEndpoint)
	}
	defer terminal.Close()
	if err := validateAttach(m); err != nil {
		return s.attachRejected(p, err)
	}
	if s.hosted && m.Instance != "" && m.Instance != s.record.UID {
		return s.attachRejected(p, tmux.ErrUnavailable)
	}
	p.Attachment = tmux.NewAttachment(terminal)
	p.approvals.ready = s.hosted && m.CanApprove
	s.mu.Lock()
	if s.frontends == nil {
		s.frontends = make(map[*tmux.Attachment]*attachment)
	}
	s.frontends[p.Attachment] = p
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.frontends, p.Attachment)
		s.mu.Unlock()
	}()
	controlClosed := make(chan struct{})
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		defer close(controlClosed)
		select {
		case <-p.Done():
			_ = c.Close()
		case <-finished:
		}
	}()
	if err := s.session.Attach(p.Attachment, s.attachmentOptions(m), func(key string, state tmux.Snapshot) error {
		return s.attachmentReady(p, m, key, state)
	}); err != nil {
		return s.attachRejected(p, err)
	}
	defer func() {
		detached, err := p.Disconnect()
		<-controlClosed
		if err != nil || result != nil && detached.Released {
			s.reportAttachmentLoss(result, err)
		}
	}()
	return s.serveAttachment(p, extra)
}

func (s *shellTask) frontend(active *tmux.Attachment) *attachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frontends[active]
}

func (s *shellTask) attachRejected(p *attachment, err error) error {
	if errors.Is(err, errTerminalEndpoint) {
		return p.controlMessage(message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
	}
	if errors.Is(err, tmux.ErrUnavailable) {
		err = fmt.Errorf("shell changed or is unavailable; refresh with sshore list")
	}
	fatal := message{Type: "fatal"}
	if s.hosted {
		fatal.SoftwareVersion = version
		switch {
		case errors.Is(err, tmux.ErrEnded):
			err = fmt.Errorf("shell is exiting; use sshore shell HOST to create a new one")
		case errors.Is(err, tmux.ErrAttached):
			err = fmt.Errorf("shell already attached; detach the other connection first")
		}
	} else if errors.Is(err, tmux.ErrEnded) || errors.Is(err, tmux.ErrAttached) {
		err = fmt.Errorf("local shell unavailable or already attached")
	}
	fatal.Error = err.Error()
	_ = p.controlMessage(fatal)
	if s.hosted {
		_ = p.control.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = readMessage(p.control)
	}
	return err
}

func (s *shellTask) attachmentOptions(m message) tmux.AttachOptions {
	return tmux.AttachOptions{
		TreeKey: m.TreeKey, Size: shellSize(m), Term: m.Term, Pin: m.Pinned,
		Existing: s.hosted && m.Action == "existing",
		Startup:  tmux.Startup{Argv: m.Argv, NoLogin: m.NoLogin, NoProfile: m.NoProfile},
	}
}

func (s *shellTask) attachmentReady(p *attachment, m message, key string, state tmux.Snapshot) error {
	instance := state.ID
	if !s.hosted {
		instance = ""
	}
	machine, _ := localMachineIdentity()
	if validMachineID(m.MachineID) && machine != m.MachineID && len(m.SourceHost) <= 256 {
		p.sourceHost = cleanText(m.SourceHost)
	}
	return p.controlMessage(message{Type: "ready", SoftwareVersion: version, Owner: taskKey(state.OwnerID),
		TreeKey: key, MachineID: machine, Pinned: state.Pinned, PinUntil: state.PinUntil, Instance: instance,
		PID: state.PID, Name: state.Name, Shell: state.Process.Shell, Program: state.Process.ProgramLine != ""})
}

func (s *shellTask) reportAttachmentLoss(cause, err error) {
	if cause != nil {
		termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Shell attachment disconnected: %s", cleanText(cause.Error()))
	}
	if err != nil {
		termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Could not release shell input: %s", cleanText(err.Error()))
	}
}

func (s *shellTask) detachAttachment(p *attachment, request message) (bool, error) {
	var retention *time.Duration
	if request.Action != "switch" {
		duration := time.Duration(request.TTLNanos)
		retention = &duration
	}
	detached, err := s.session.Detach(p.Attachment, retention)
	if _, failed := errors.AsType[tmux.PinFailure](err); failed {
		return false, p.controlMessage(message{Type: "pin_failed", Error: err.Error()})
	}
	p.approvals.disable()
	if err != nil {
		s.reportAttachmentLoss(nil, err)
	} else if detached.Released {
		err = p.finishDetach(message{Type: "detached", Name: s.record.Name, Pinned: detached.Pinned, PinUntil: detached.PinUntil})
	}
	return true, err
}

// The control loop translates messages while tmux runs the terminal byte stream.
func (s *shellTask) serveAttachment(p *attachment, extra func(*attachment, message) error) error {
	for {
		request, err := readMessage(p.control)
		if err != nil {
			return err
		}
		if request.Type == "detach" {
			finished, err := s.detachAttachment(p, request)
			if finished || err != nil {
				return err
			}
			continue
		}
		if err := s.attachmentControl(p, request, extra); err != nil {
			return err
		}
	}
}

func (s *shellTask) attachmentControl(p *attachment, request message, extra func(*attachment, message) error) error {
	switch request.Type {
	case "input_barrier":
		ready, err := p.DrainInput(request.TerminalOffset)
		if err != nil || !ready {
			return err
		}
		return p.controlMessage(message{Type: "input_drained"})
	case "resize":
		if err := s.session.ResizeAttached(p.Attachment, shellSize(request)); err != nil {
			return fmt.Errorf("resize terminal: %w", err)
		}
		if s.hosted {
			return p.controlMessage(message{Type: "resized", terminalDisplay: terminalDisplay{Cols: request.Cols, Rows: request.Rows}})
		}
		return nil
	case "redraw":
		s.session.Redraw(p.Attachment)
		return p.controlMessage(message{Type: "redrawn"})
	case "session_targets":
		return p.controlMessage(s.sessionTargets(request))
	case "console_files":
		return p.controlMessage(consoleFileListing(request))
	case "console_inventory":
		return p.controlMessage(consoleInventory(request, s.treeIdentity()))
	default:
		return extra(p, request)
	}
}

func (s *shellTask) holdTree(conn net.Conn, m message) error {
	if checkPeerVersion(m.SoftwareVersion, "client") != nil {
		return writeMessage(conn, message{Type: "error", Error: "current shell attachment changed"})
	}
	release, ok := s.session.Hold(m.TreeKey)
	if !ok {
		return writeMessage(conn, message{Type: "error", Error: "current shell attachment changed"})
	}
	defer release()
	if err := writeMessage(conn, message{Type: "tree_held"}); err != nil {
		return err
	}
	// EOF releases this reservation, including on client or SSH loss.
	var b [1]byte
	_, _ = conn.Read(b[:])
	return nil
}

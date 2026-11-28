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
	a.FinishOutput(func(offset uint64, incomplete bool) {
		exit.TerminalOffset, exit.OutputIncomplete = offset, incomplete
		_ = a.controlMessage(exit)
	})
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

func (s *tmuxTask) attach(c net.Conn, m message, extra func(*attachment, message) error) error {
	// The initial request deadline must not expire an attached shell.
	_ = c.SetReadDeadline(time.Time{})
	p := &attachment{Attachment: tmux.NewAttachment(c), control: c}
	return s.runtime.ServeAttachment(p, s.attachmentRequest(p, m), func(key string, state tmux.Snapshot[*attachment]) error {
		return s.attachmentReady(p, m, key, state)
	}, func(err error) error {
		if err != nil {
			return s.attachRejected(p, err)
		}
		return s.serveAttachment(p, extra)
	})
}

func (s *tmuxTask) attachRejected(p *attachment, err error) error {
	if errors.Is(err, tmux.ErrTerminalEndpoint) {
		return p.controlMessage(message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
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

func (s *tmuxTask) attachmentRequest(p *attachment, m message) tmux.AttachRequest {
	req := tmux.AttachRequest{TerminalToken: m.Token, ValidationError: validateAttach(m)}
	if req.ValidationError != nil {
		return req
	}
	if s.hosted && m.Instance != "" && m.Instance != s.record.UID {
		req.ValidationError = fmt.Errorf("shell changed or is unavailable; refresh with sshore list")
		return req
	}
	req.TreeKey, req.Size, req.Pin = m.TreeKey, shellSize(m), m.Pinned
	req.Existing, req.RetryUnstarted = s.hosted && m.Action == "existing", s.hosted
	req.StartupOptions = m.NoLogin || m.NoProfile || len(m.Argv) != 0
	if owned, ok := p.control.(*ownedShellConnection); ok {
		req.OwnerToken = owned.token
	}
	if !req.Existing {
		req.Launch, req.LaunchError = s.shellLaunch(m)
	}
	return req
}

func (s *tmuxTask) attachmentReady(p *attachment, m message, key string, state tmux.Snapshot[*attachment]) error {
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

func (s *tmuxTask) reportAttachmentLoss(cause, err error) {
	if cause != nil {
		termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Shell attachment disconnected: %s", cleanText(cause.Error()))
	}
	if err != nil {
		termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Could not release shell input: %s", cleanText(err.Error()))
	}
}

func (s *tmuxTask) detachAttachment(p *attachment, request message) (bool, error) {
	var retention *time.Duration
	if request.Action != "switch" {
		duration := time.Duration(request.TTLNanos)
		retention = &duration
	}
	detached, err := s.runtime.Detach(p, retention)
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
func (s *tmuxTask) serveAttachment(p *attachment, extra func(*attachment, message) error) error {
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

func (s *tmuxTask) attachmentControl(p *attachment, request message, extra func(*attachment, message) error) error {
	switch request.Type {
	case "input_barrier":
		ready, err := p.DrainInput(request.TerminalOffset)
		if err != nil || !ready {
			return err
		}
		return p.controlMessage(message{Type: "input_drained"})
	case "resize":
		if err := s.runtime.ResizeAttached(p, shellSize(request)); err != nil {
			return fmt.Errorf("resize terminal: %w", err)
		}
		if s.hosted {
			return p.controlMessage(message{Type: "resized", terminalDisplay: terminalDisplay{Cols: request.Cols, Rows: request.Rows}})
		}
		return nil
	case "redraw":
		s.runtime.Redraw(p)
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

func (s *tmuxTask) holdTree(conn net.Conn, m message) error {
	if checkPeerVersion(m.SoftwareVersion, "client") != nil {
		return writeMessage(conn, message{Type: "error", Error: "current shell attachment changed"})
	}
	release, ok := s.runtime.Hold(m.TreeKey)
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

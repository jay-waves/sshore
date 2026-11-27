package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
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

func newAttachment(control attachmentControl, terminal net.Conn) *attachment {
	return &attachment{Attachment: tmux.NewAttachment(control, terminal), control: control}
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

// FinishExit follows terminal output, allowing bounded consumption before close.
func (a *attachment) finishExit(exit message) {
	a.AfterOutput(func(offset uint64, incomplete bool) {
		exit.TerminalOffset, exit.OutputIncomplete = offset, incomplete
		_ = a.controlMessage(exit)
	})
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
func (a *attachment) FinishDetach(reply message) error {
	defer a.Close()
	if err := a.controlMessage(reply); err != nil {
		return err
	}
	_ = a.control.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := expectDetachMessage(a.control, "bye")
	return err
}

func (s *tmuxTask) attach(c net.Conn, m message, extra func(*attachment, message) error) error {
	terminal := s.endpoints.take(m.Token)
	if terminal == nil {
		return writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: "terminal endpoint expired or already claimed"})
	}
	// The initial request deadline must not expire an attached shell.
	_ = c.SetReadDeadline(time.Time{})
	p := newAttachment(c, terminal)
	if err := s.attachManaged(p, m); err != nil {
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
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = readMessage(c)
		}
		p.Close()
		return err
	}
	return s.serveAttachment(p, extra)
}

// Publish the input lease after startup, the ready message and terminal replay.
// Hosted shells obtain their owner context from the authenticated attachment.
func (s *tmuxTask) attachManaged(p *attachment, m message) error {
	if err := validateAttach(m); err != nil {
		return err
	}
	s.terminalMu.Lock()
	s.mu.Lock()
	if err := s.checkRemoteOwnerLocked(p.control); err != nil {
		s.mu.Unlock()
		s.terminalMu.Unlock()
		return err
	}
	initial := !s.runtime.Started()
	if s.killing || s.hosted && (m.Instance != "" && m.Instance != s.record.UID || initial && m.Action == "existing") {
		s.mu.Unlock()
		s.terminalMu.Unlock()
		return fmt.Errorf("shell changed or is unavailable; refresh with sshore list")
	}
	prepared := false
	parent, err := s.runtime.Attach(p, m.TreeKey, s.remoteOwner != "", func() bool {
		s.reapUnretainedLocked()
		return !s.runtime.Exited() && !s.killing
	}, func(key string) error {
		prepared = true
		return s.prepareAttachment(p, m, initial, key)
	})
	if parent != nil {
		s.watchParent(parent)
	}
	if err == nil && !initial {
		s.runtime.Redraw(p)
	}
	failedInitial := err != nil && initial && prepared && !errors.Is(err, tmux.ErrAttached) && !errors.Is(err, tmux.ErrEnded)
	abort := failedInitial && (!s.hosted || s.runtime.Started())
	if abort {
		s.killing, s.exitReason = true, "initial attachment failed"
		s.runtime.End()
	}
	s.mu.Unlock()
	s.terminalMu.Unlock()
	s.publishTreeState()
	if abort {
		if s.hosted {
			s.close()
		} else {
			_ = s.stopTerminal()
		}
	}
	return err
}

// The caller holds terminalMu and mu until input ownership is published.
func (s *tmuxTask) prepareAttachment(p *attachment, m message, initial bool, key string) error {
	if initial {
		if err := s.startManagedShell(m); err != nil {
			return err
		}
		if m.Pinned {
			if err := s.pinLocked(0); err != nil {
				return err
			}
		}
	} else {
		if m.NoLogin || m.NoProfile || len(m.Argv) != 0 {
			return fmt.Errorf("startup options require a new managed shell")
		}
		if err := s.runtime.Resize(shellSize(m)); err != nil {
			return err
		}
	}
	instance := s.record.UID
	if !s.hosted {
		instance = ""
	}
	machine, _ := localMachineIdentity()
	if validMachineID(m.MachineID) && machine != m.MachineID && len(m.SourceHost) <= 256 {
		p.sourceHost = cleanText(m.SourceHost)
	}
	pinned, until := s.runtime.PinState()
	if err := p.controlMessage(message{Type: "ready", SoftwareVersion: version, Owner: s.parentIdentityLocked(), TreeKey: key, MachineID: machine, Pinned: pinned, PinUntil: until, Instance: instance, PID: s.runtime.PID(), Name: s.record.Name, Shell: s.shellName, Program: s.program.Load()}); err != nil {
		return err
	}
	if data := s.runtime.Replay(); len(data) > 0 {
		if err := p.Output(data); err != nil {
			return err
		}
	}
	return nil
}

// A descendant's terminal reservation also changes the waiting state of its ancestors.
func (s *tmuxTask) publishTreeState() {
	for task := s; task != nil; task = task.parentTask() {
		task.registry.inventoryChanged(task)
	}
}

func (s *tmuxTask) releaseAttachment(p *attachment, cause error) (message, error) {
	s.mu.Lock()
	disconnected := cause != nil && !s.runtime.Exited() && s.runtime.Active() == p
	// Hold mu through cancellation so a successor cannot inherit an old write.
	detached, err := s.runtime.ReleaseInput(p)
	if err != nil {
		s.exitReason = "terminal input cancellation failed"
	}
	s.reapUnretainedLocked()
	var reply message
	if detached && err == nil {
		pinned, until := s.runtime.PinState()
		reply = message{Type: "detached", Name: s.record.Name, Pinned: pinned, PinUntil: until}
	}
	s.mu.Unlock()
	s.publishTreeState()
	if disconnected {
		termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Shell attachment disconnected: %s", cleanText(cause.Error()))
	}
	if err != nil {
		termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Could not release shell input: %s", cleanText(err.Error()))
		s.close()
	}
	return reply, err
}

func (s *tmuxTask) detachAttachment(p *attachment, request message) (bool, error) {
	if request.Action != "switch" {
		s.mu.Lock()
		err := s.pinLocked(time.Duration(request.TTLNanos))
		s.mu.Unlock()
		if err != nil {
			return false, p.controlMessage(message{Type: "pin_failed", Error: err.Error()})
		}
	}
	p.approvals.disable()
	reply, err := s.releaseAttachment(p, nil)
	if err == nil && reply.Type != "" {
		err = p.FinishDetach(reply)
	}
	return true, err
}

// Only this loop reads control frames. The byte stream runs independently,
// with the same input lease until detach or disconnect releases it.
func (s *tmuxTask) serveAttachment(p *attachment, extra func(*attachment, message) error) (result error) {
	released := false
	inputDone := p.PumpInput()
	defer func() {
		p.Close()
		if !released {
			_, _ = s.releaseAttachment(p, result)
		}
		<-inputDone
	}()
	for {
		request, err := readMessage(p.control)
		if err != nil {
			return err
		}
		if request.Type == "detach" {
			released, err = s.detachAttachment(p, request)
			if released || err != nil {
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
		if err := s.resizeAttachment(p, shellSize(request)); err != nil {
			return fmt.Errorf("resize terminal: %w", err)
		}
		if s.hosted {
			return p.controlMessage(message{Type: "resized", terminalDisplay: terminalDisplay{Cols: request.Cols, Rows: request.Rows}})
		}
		return nil
	case "redraw":
		s.redrawAttachment(p)
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

func (s *tmuxTask) resizeAttachment(p *attachment, size pty.Size) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtime.ResizeAttached(p, size)
}

func (s *tmuxTask) redrawAttachment(p *attachment) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime.Redraw(p)
}

func (s *tmuxTask) holdTree(conn net.Conn, m message) error {
	if checkPeerVersion(m.SoftwareVersion, "client") != nil {
		return writeMessage(conn, message{Type: "error", Error: "current shell attachment changed"})
	}
	release, ok := s.runtime.Hold(m.TreeKey)
	if !ok {
		return writeMessage(conn, message{Type: "error", Error: "current shell attachment changed"})
	}
	s.publishTreeState()
	defer func() {
		release()
		s.publishTreeState()
	}()
	if err := writeMessage(conn, message{Type: "tree_held"}); err != nil {
		return err
	}
	// EOF releases this reservation, including on client or SSH loss.
	var b [1]byte
	_, _ = conn.Read(b[:])
	return nil
}

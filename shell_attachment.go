package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/termui"
)

// attachment owns the two connections and the terminal byte counters.
type attachment struct {
	sourceHost          string
	claim               *treeClaim
	control             attachmentControl
	terminal            net.Conn
	done                chan struct{}
	closeOnce           sync.Once
	outputBytes         uint64
	outputErr           error
	inputBytes          atomic.Uint64
	inputProgress       chan struct{}
	controlMu, outputMu sync.Mutex
	input               *shellInput
	approvals           approvalExchanges
}

func newAttachment(control attachmentControl, terminal net.Conn) *attachment {
	return &attachment{control: control, terminal: terminal, done: make(chan struct{}), inputProgress: make(chan struct{}, 1)}
}
func (a *attachment) Done() <-chan struct{} { return a.done }
func (a *attachment) Close() {
	a.closeOnce.Do(func() {
		close(a.done)
		_ = a.terminal.Close()
		_ = a.control.Close()
	})
}
func (a *attachment) controlMessage(m message) error {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	return streamMessage(a.control, m)
}

// Replace the interactive launcher with an in-process request. The existing
// handoff owns confirmation, rollback and the task lifetime.
func serveChildShortcut(p *attachment, done <-chan struct{}, serve func(net.Conn)) {
	client, server := net.Pipe()
	defer client.Close()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		defer server.Close()
		serve(server)
	}()
	go func() {
		select {
		case <-done:
			client.Close()
		case <-finished:
		}
	}()
	accepted := false
	for {
		m, err := readMessage(client)
		if err != nil {
			if !accepted {
				_ = p.controlMessage(message{Type: "sh_child_error", Error: "shell handoff ended before confirmation"})
			}
			return
		}
		if m.Type == "handoff_accepted" {
			accepted = true
		}
		if m.Error != "" {
			_ = p.controlMessage(message{Type: "sh_child_error", Error: m.Error})
			return
		}
	}
}

func (a *attachment) Output(data []byte) error {
	a.outputMu.Lock()
	defer a.outputMu.Unlock()
	if a.outputErr != nil {
		return a.outputErr
	}
	_ = a.terminal.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := writeFull(a.terminal, data)
	_ = a.terminal.SetWriteDeadline(time.Time{})
	if err != nil {
		a.outputErr = err
		_ = a.terminal.Close()
		return err
	}
	a.outputBytes += uint64(len(data))
	return nil
}

// FinishExit follows terminal output, allowing bounded consumption before close.
func (a *attachment) finishExit(exit message) {
	a.outputMu.Lock()
	exit.TerminalOffset, exit.OutputIncomplete = a.outputBytes, a.outputErr != nil
	_ = a.controlMessage(exit)
	a.outputMu.Unlock()
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

// DrainInput acknowledges only bytes accepted by the PTY input lease. The
// counter preserves detach ordering across independent control/data channels.
func (a *attachment) DrainInput(offset uint64) error {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for a.inputBytes.Load() < offset {
		select {
		case <-a.inputProgress:
		case <-a.Done():
			return io.ErrClosedPipe
		case <-timer.C:
			// Leave the barrier unacknowledged so the client can report
			// uncertain input and send detach without losing the shell.
			return nil
		}
	}
	return a.controlMessage(message{Type: "input_drained"})
}
func (a *attachment) ReadInput(input *shellInput, active func() bool) {
	buf := make([]byte, 32*1024)
	for {
		n, err := a.terminal.Read(buf)
		if n > 0 {
			if !active() {
				return
			}
			if e := writeFull(input, buf[:n]); e != nil {
				if !input.Stopped() {
					a.Close()
				}
				return
			}
			a.inputBytes.Add(uint64(n))
			select {
			case a.inputProgress <- struct{}{}:
			default:
			}
		}
		if err != nil {
			a.Close()
			return
		}
	}
}

type shellInput struct {
	terminal *pty.Terminal
	mu       sync.Mutex
	stopped  atomic.Bool
	once     sync.Once
	stopErr  error
}

func (l *shellInput) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped.Load() {
		return 0, os.ErrClosed
	}
	return l.terminal.Write(p)
}
func (l *shellInput) Stop() error {
	l.once.Do(func() {
		l.stopped.Store(true)
		l.stopErr = l.terminal.SetWriteDeadline(time.Now())
		// A device without usable cancellation cannot safely transfer input ownership.
		if l.stopErr != nil {
			_ = l.terminal.Close()
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.stopErr == nil {
			l.stopErr = l.terminal.SetWriteDeadline(time.Time{})
		}
	})
	return l.stopErr
}

// Stopped distinguishes owner cancellation from an unexpected terminal failure.
func (l *shellInput) Stopped() bool { return l.stopped.Load() }

func (s *shellTask) attach(c net.Conn, extra func(*attachment, message) error) error {
	terminal, m, err := acceptAttachment(c, &s.endpoints)
	if err != nil {
		return err
	}
	p := newAttachment(c, terminal)
	if err := s.attachManaged(p, m); err != nil {
		fatal := message{Type: "fatal"}
		if s.hosted {
			fatal.SoftwareVersion = version
			switch {
			case errors.Is(err, errShellExited):
				err = fmt.Errorf("shell is exiting; use sshore shell HOST: to create a new one")
			case errors.Is(err, errShellAttached):
				err = fmt.Errorf("shell already attached; detach the other connection first")
			}
		} else if errors.Is(err, errShellExited) || errors.Is(err, errShellAttached) {
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
	return s.serveAttachment(p, c, extra)
}

// Publish the input lease after startup, the ready message and terminal replay.
// Hosted shells obtain their owner context from the authenticated attachment.
func (s *shellTask) attachManaged(p *attachment, m message) error {
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
	initial := s.terminal == nil
	if s.killing || s.hosted && (m.Instance != "" && m.Instance != s.record.UID || initial && m.Action == "existing") {
		s.mu.Unlock()
		s.terminalMu.Unlock()
		return fmt.Errorf("shell changed or is unavailable; refresh with sshore list")
	}
	err := s.forest.attach(s, m.TreeKey, func(claim *treeClaim) error {
		p.claim = claim
		return s.activateAttachment(p, m, initial)
	})
	failedInitial := err != nil && initial && p.claim != nil && !errors.Is(err, errShellAttached) && !errors.Is(err, errShellExited)
	if failedInitial && s.hosted && s.terminal != nil {
		s.endTerminal()
		s.exitReason = "initial attachment failed"
		_ = s.terminal.Close()
		s.finishLocked()
	} else if failedInitial && !s.hosted {
		s.killing, s.exitReason = true, "initial attachment failed"
		s.endTerminal()
	}
	s.mu.Unlock()
	s.terminalMu.Unlock()
	if failedInitial && !s.hosted {
		_ = s.stopTerminal()
	}
	return err
}

// The caller holds terminalMu and mu until input ownership is published.
func (s *shellTask) activateAttachment(p *attachment, m message, initial bool) error {
	if s.active != nil {
		return errShellAttached
	}
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
		if m.NoLogin || m.NoProfile {
			return fmt.Errorf("startup options require a new managed shell")
		}
		if err := s.terminal.Resize(shellSize(m)); err != nil {
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
	if err := p.controlMessage(message{Type: "ready", Owner: s.parentIdentityLocked(), TreeKey: p.claim.key, MachineID: machine, Pinned: s.pinned, PinUntil: s.pinUntil, Instance: instance, PID: s.pid, Name: s.record.Name, Shell: s.shellName}); err != nil {
		return err
	}
	if data := s.modes.Replay(); len(data) > 0 {
		if err := p.Output(data); err != nil {
			return err
		}
	}
	p.input = &shellInput{terminal: s.terminal}
	s.active = p
	if !initial {
		pty.RequestRedraw(s.terminal)
	}
	return nil
}

// Hold mu through Stop so no successor can acquire the terminal's writer.
func (s *shellTask) detachTerminal(p *attachment) (bool, error) {
	active := s.active == p
	if p.input == nil || !active && !s.exited {
		return false, nil
	}
	input := p.input
	p.input = nil
	err := input.Stop()
	// An exited shell has no successor, but its input worker must still join.
	if !active {
		return false, nil
	}
	if err != nil {
		s.endTerminal()
		return false, err
	}
	s.active = nil
	return true, nil
}

// Once attached, only this loop reads control frames. The host records release
// under the shell lock before a successor can take input.
func (s *shellTask) serveAttachment(p *attachment, control io.Reader, extra func(*attachment, message) error) (result error) {
	released := false
	release := func() (string, error) {
		s.mu.Lock()
		disconnected := !released && !s.exited && s.active == p && result != nil
		detached, err := s.detachTerminal(p)
		s.forest.releaseInput(p.claim)
		var name string
		if err != nil && s.hosted {
			s.exitReason = "terminal input cancellation failed"
			s.finishLocked()
		} else if detached && err == nil {
			name = s.record.Name
		}
		s.reapUnretainedLocked()
		s.mu.Unlock()
		if disconnected {
			termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Shell attachment disconnected: %s", cleanText(result.Error()))
		}
		if err != nil {
			termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Error, "Could not release shell input: %s", cleanText(err.Error()))
		}
		if err != nil && !s.hosted {
			s.close()
		}
		return name, err
	}
	inputDone := make(chan struct{})
	defer func() {
		p.Close()
		if !released {
			_, _ = release()
		}
		<-inputDone
	}()
	input := p.input
	go func() {
		defer close(inputDone)
		p.ReadInput(input, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.active == p
		})
	}()
	for {
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		switch m.Type {
		case "detach":
			if m.Action != "switch" {
				s.mu.Lock()
				err := s.pinLocked(time.Duration(m.TTLNanos))
				s.mu.Unlock()
				if err != nil {
					if err := p.controlMessage(message{Type: "pin_failed", Error: err.Error()}); err != nil {
						return err
					}
					continue
				}
			}
			p.approvals.disable()
			released = true
			name, err := release()
			if err == nil && name != "" {
				s.mu.Lock()
				reply := message{Type: "detached", Name: name, Pinned: s.pinned, PinUntil: s.pinUntil}
				s.mu.Unlock()
				return p.FinishDetach(reply)
			}
			return err
		case "input_barrier":
			err = p.DrainInput(m.TerminalOffset)
		case "resize":
			s.mu.Lock()
			if s.active == p {
				err = s.terminal.Resize(shellSize(m))
			}
			s.mu.Unlock()
			if err != nil {
				return fmt.Errorf("resize terminal: %w", err)
			}
			if s.hosted {
				err = p.controlMessage(message{Type: "resized", Cols: m.Cols, Rows: m.Rows})
			}
		case "session_targets":
			err = p.controlMessage(s.sessionTargets(m))
		case "console_inventory":
			s.mu.Lock()
			current := s.treeIdentity()
			s.mu.Unlock()
			err = p.controlMessage(consoleInventory(m, current))
		case "redraw":
			s.terminalMu.Lock()
			s.mu.Lock()
			if s.active == p {
				pty.RequestRedraw(s.terminal)
			}
			s.mu.Unlock()
			s.terminalMu.Unlock()
			err = p.controlMessage(message{Type: "redrawn"})
		default:
			err = extra(p, m)
		}
		if err != nil {
			return err
		}
	}
}

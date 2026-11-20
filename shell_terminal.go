package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
	"github.com/jay-waves/sshore/internal/pty"
)

type shellTerminal struct {
	endpoints  terminalEndpoints
	mu         sync.Mutex // Protect shell state and attachment ownership.
	terminalMu sync.Mutex // Serialize replay and redraw with live output. Acquire before mu.
	renameMu   sync.Mutex // Keep each shell's history renames in live update order.
	terminal   *pty.Terminal
	process    processIdentity
	modes      pty.InputState
	active     *attachment
	exited     bool
}

// change runs with both the task table and shell locked and returns the tree
// identity to persist. The rename lock preserves write order for this shell.
func (s *shellTerminal) renameTask(tasks *daemonTasks, uid, name string, change func() (string, error)) error {
	if err := validAlias(name); err != nil {
		return err
	}
	s.renameMu.Lock()
	defer s.renameMu.Unlock()
	tasks.mu.Lock()
	if err := tasks.checkNameLocked(uid, name, time.Now()); err != nil {
		tasks.mu.Unlock()
		return err
	}
	s.mu.Lock()
	identity, err := change()
	s.mu.Unlock()
	tasks.mu.Unlock()
	if err == nil && identity != "" {
		renameShellTree(identity, name)
	}
	return err
}

var (
	errShellExited   = errors.New("session is exiting")
	errShellAttached = errors.New("session already attached")
)

// Call with mu held.
func (s *shellTerminal) endTerminal() *attachment {
	p := s.active
	s.active, s.exited = nil, true
	return p
}

type shellInput interface {
	io.Writer
	Stopped() bool
	Stop() error
}

func shellSize(m message) pty.Size {
	return pty.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}
}

// The caller holds mu while publishing the terminal. The exit handler owns
// task-specific state and exit reporting.
func (s *shellTerminal) startTerminal(path string, args, env []string, dir string, m message, exited func(error, int)) error {
	t, err := pty.Start(pty.Config{Path: path, Args: args, Env: env, Dir: dir, Size: shellSize(m)})
	if err != nil {
		return err
	}
	pid := t.PID()
	identity, err := proc.LiveIdentity(pid)
	if err != nil {
		t.Close()
		return err
	}
	s.terminal = t
	s.process = processIdentity{PID: pid, Start: identity}
	go s.pumpTerminal(t, exited)
	return nil
}

func (s *shellTerminal) pumpTerminal(t *pty.Terminal, exited func(error, int)) {
	buf := make([]byte, 8192)
	for {
		n, err := t.Read(buf)
		if n > 0 {
			s.terminalMu.Lock()
			s.modes.Observe(buf[:n])
			// Keep live output ordered without holding the shell lock during I/O.
			s.mu.Lock()
			p := s.active
			s.mu.Unlock()
			if p != nil && p.Output(buf[:n]) != nil {
				p.Close()
			}
			s.terminalMu.Unlock()
		}
		if err != nil {
			break
		}
	}
	err := t.Wait()
	code := 0
	if err != nil {
		code = 1
		if e, ok := errors.AsType[interface {
			error
			ExitCode() int
		}](err); ok {
			code = e.ExitCode()
		}
	}
	exited(err, code)
}

// Caller holds both host locks. The attachment acquires ownership only after
// startup, terminal setup and the ready handshake have succeeded.
func (s *shellTerminal) attachTerminal(p *attachment, m message, start func(message) error, ready func() error) error {
	if s.exited {
		return errShellExited
	}
	if s.active != nil {
		return errShellAttached
	}
	initial := s.terminal == nil
	if initial {
		if err := start(m); err != nil {
			return err
		}
	} else if m.NoLogin || m.NoProfile {
		return fmt.Errorf("startup options require a new managed shell")
	}
	if err := s.terminal.Resize(shellSize(m)); err != nil {
		return err
	}
	lease := pty.NewInputLease(s.terminal)
	err := ready()
	if err == nil {
		if data := s.modes.Replay(); len(data) > 0 {
			err = p.Output(data)
		}
	}
	if err != nil {
		lease.Stop()
		return err
	}
	p.input = lease
	s.active = p
	if !initial {
		pty.RequestRedraw(s.terminal, s.process.PID)
	}
	return nil
}

// Hold mu through Stop so no successor can acquire the terminal's writer.
func (s *shellTerminal) detachTerminal(p *attachment) (bool, error) {
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
// under the shell lock, and may return cleanup that needs to run outside it.
func (s *shellTerminal) serveAttachment(p *attachment, control io.Reader, resizeAck bool, onRelease func(error) (string, func()), extra func(message) error) error {
	release := func() (string, error) {
		s.mu.Lock()
		detached, err := s.detachTerminal(p)
		var name string
		var cleanup func()
		if detached || err != nil {
			name, cleanup = onRelease(err)
		}
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return name, err
	}
	released := false
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
			p.approvals.disable()
			released = true
			name, err := release()
			if err == nil && name != "" {
				return p.FinishDetach(name)
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
			if resizeAck {
				err = p.controlMessage(message{Type: "resized", Cols: m.Cols, Rows: m.Rows})
			}
		case "redraw":
			s.terminalMu.Lock()
			s.mu.Lock()
			if s.active == p {
				pty.RequestRedraw(s.terminal, s.process.PID)
			}
			s.mu.Unlock()
			s.terminalMu.Unlock()
			err = p.controlMessage(message{Type: "redrawn"})
		default:
			err = extra(m)
		}
		if err != nil {
			return err
		}
	}
}

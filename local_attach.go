package main

import (
	"fmt"
	"net"

	shellmux "github.com/jay-waves/sshore/internal/mux"
)

func runLocalShell(record localSessionRecord, env []string, ready func(localSessionRecord) error) error {
	s := &localShell{done: make(chan struct{}), environment: env}
	defer s.close()
	return serveLocalWorker(s, record, ready, func(c net.Conn, m message) error {
		switch m.Type {
		case "status":
			s.mu.Lock()
			reply := message{Type: "session", Name: s.record.Name, SoftwareVersion: version, Attached: s.lifecycle.Active() != nil}
			if s.terminal != nil {
				reply.PID = s.terminal.PID()
			}
			s.mu.Unlock()
			return writeMessage(c, reply)
		case "rename":
			err := s.rename(m.Name)
			reply := message{Type: "renamed", Name: m.Name}
			if err != nil {
				reply.Type = "error"
				reply.Error = err.Error()
			}
			return writeMessage(c, reply)
		case "detach":
			s.mu.Lock()
			p := s.lifecycle.Active()
			s.mu.Unlock()
			if p == nil {
				return writeMessage(c, message{Type: "detached"})
			}
			if err := p.controlMessage(message{Type: "detach_requested"}); err != nil {
				return err
			}
			return writeMessage(c, message{Type: "detach_requested"})
		case "cancel":
			_ = writeMessage(c, message{Type: "cancelled"})
			s.close()
			return nil
		case "mux":
			if m.SoftwareVersion != version {
				return fmt.Errorf("local version mismatch")
			}
			if err := writeMessage(c, message{Type: "mux_ready", SoftwareVersion: version}); err != nil {
				return err
			}
			return s.attach(c)
		default:
			return fmt.Errorf("unexpected local request")
		}
	}, s.done)
}

func (s *localShell) attach(c net.Conn) error {
	mx, control, terminal, m, err := acceptMux(c)
	if err != nil {
		return err
	}
	defer mx.Close()
	p := &attachment{control: control, terminal: terminal, mux: mx}
	if err := validateAttach(m); err != nil {
		return err
	}
	s.mu.Lock()
	if s.lifecycle.Active() != nil || s.lifecycle.Exited() {
		s.mu.Unlock()
		_ = p.controlMessage(message{Type: "fatal", Error: "local shell unavailable or already attached"})
		return fmt.Errorf("local shell unavailable")
	}
	initial := s.terminal == nil
	if initial {
		err = s.start(m)
	}
	if err == nil {
		err = s.terminal.Resize(shellmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)})
	}
	if err == nil {
		err = p.controlMessage(message{Type: "ready", Name: s.record.Name, PID: s.terminal.PID()})
	}
	if err == nil {
		err = p.output(message{Type: "output", Data: s.modes.Replay()})
	}
	if err != nil {
		s.mu.Unlock()
		if initial {
			s.close()
		}
		return err
	}
	t := s.terminal
	lease, err := shellmux.NewTerminalInput(t)
	if err != nil {
		s.mu.Unlock()
		if initial {
			s.close()
		}
		return err
	}
	if err = s.lifecycle.Attach(p, func() error { return nil }); err != nil {
		s.mu.Unlock()
		lease.Stop()
		return err
	}
	p.input = lease
	s.mu.Unlock()
	// Losing a local client releases attachment ownership, not the daemon-owned
	// terminal. Stop its writer before another client can attach.
	defer func() {
		lease.Stop()
		s.mu.Lock()
		s.lifecycle.Detach(p)
		s.mu.Unlock()
	}()
	go p.readInput(&s.lifecycle)
	if !initial {
		// Output ownership must be installed before requesting a repaint.
		redrawLocalTerminal(t)
	}

	for {
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		switch m.Type {
		case "detach":
			s.mu.Lock()
			if err := lease.Stop(); err != nil {
				s.mu.Unlock()
				return err
			}
			s.lifecycle.Detach(p)
			name := s.record.Name
			s.mu.Unlock()
			shellmux.FinishDetach(control, func() error { return p.controlMessage(message{Type: "detached", Name: name}) }, func() { mx.Close() })
			return nil
		case "session_targets":
			if err = p.controlMessage(s.targets(m)); err != nil {
				return err
			}
		case "resize":
			if err = t.Resize(shellmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}); err != nil {
				return err
			}
		case "redraw":
			redrawLocalTerminal(t)
			if err = p.controlMessage(message{Type: "redrawn"}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected local control request")
		}
	}
}

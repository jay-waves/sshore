//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type remoteShell struct {
	shellTask
	shellRoutes map[string]*shellRoute
}

func (s *remoteShell) start(m message) error {
	env := s.shellEnvironment()
	path := localEnvironmentValue(env, "SHELL")
	if path == "" {
		path = "/bin/bash"
	}
	exe, _ := os.Executable()
	return s.startManagedShell(path, hostedShellEnvironment(env, m.Term, s.record.UID, exe), m)
}
func (s *remoteShell) attach(p *attachment, m message) error {
	err := s.attachManaged(p, m, s.start)

	if errors.Is(err, errShellExited) {
		return fmt.Errorf("shell is exiting; use sshore shell HOST: to create a new one")
	}
	if errors.Is(err, errShellAttached) {
		return fmt.Errorf("shell already attached; detach the other connection first")
	}
	return err
}

func (s *remoteShell) serveControl(c net.Conn) error {
	terminal, m, err := acceptAttachment(c, &s.endpoints)
	if err != nil {
		return err
	}
	p := newAttachment(c, terminal)
	err = s.attach(p, m)
	if err != nil {
		_ = p.controlMessage(message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = readMessage(c)
		p.Close()
		return err
	}
	return s.serveAttachment(p, c, true, func(err error) (string, func()) {
		if err != nil {
			s.exitReason = "terminal input cancellation failed"
			s.finishLocked()
			return "", nil
		}
		return s.record.Name, nil
	}, func(m message) error {
		switch m.Type {
		case "sh_child":
			s.mu.Lock()
			req := &localDaemonRequest{Record: localShellRecord{Directory: s.record.Directory}, Env: append([]string(nil), s.environment...), Shell: &shellConfig{Command: "sh", Local: true, Pin: m.Pinned}}
			s.mu.Unlock()
			go serveChildShortcut(p, s.done, func(c net.Conn) {
				s.requestShellRoute(c, message{SoftwareVersion: version, LocalRequest: req})
			})
		case "approval_ready":
			p.approvals.enable()
		case "approval_handoff", "approval_rejected":
			p.approvals.reply(m)
		default:
			return fmt.Errorf("unexpected control message: %s", m.Type)
		}
		return nil
	})
}

func (s *remoteShell) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	first, err := readMessage(c)
	if err != nil {
		return
	}
	s.handleTask(c, first)
}

func (s *remoteShell) handleTask(c net.Conn, first message) {
	if first.Type == "shell_owner_access" {
		owned := &ownedShellConnection{Conn: c, token: first.Token}
		s.mu.Lock()
		allowed := s.remoteOwner != "" && s.checkRemoteOwnerLocked(owned) == nil
		s.mu.Unlock()
		if !allowed || first.SoftwareVersion != version || first.Instance != s.record.UID {
			_ = writeMessage(c, message{Type: "error", Error: "shell owner route unavailable"})
			return
		}
		if err := writeMessage(c, message{Type: "shell_owner_access_ready", SoftwareVersion: version}); err != nil {
			return
		}
		var err error
		first, err = readMessage(c)
		if err != nil {
			return
		}
		c = owned
	}
	if handled, _ := s.serveCommand(c, first, s.serveControl, s.status); handled {
		return
	}
	switch first.Type {
	case "route_mount", "route_forward", "route_files":
		s.serveRoutedResource(c, first)
	case "shell_connect":
		_ = s.connectRouteShell(c, first)
	case "shell_route_confirm":
		s.confirmShellRoute(c, first)
	case "shell_route_request":
		s.requestShellRoute(c, first)
	case "shell_route_connect":
		s.connectShellRoute(c, first)
	case "approval_check":
		reply := message{Type: "approval_checked", SoftwareVersion: version}
		if err := s.checkApprovalSource(first); err != nil {
			reply.Type = "error"
			reply.Error = err.Error()
		}
		_ = writeMessage(c, reply)
	case "approval_request":
		if err := s.requestApproval(c, first); err != nil {
			_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
		}
	default:
		_ = writeMessage(c, message{Type: "fatal", Error: "unsupported connection; update sshore on both computers"})
	}
}
func (s *remoteShell) status(detail string) message {
	view, group := s.hostedView(detail)
	if detail != "health" {
		view.Directory = sessionDirectory(group, view.PID)
	}
	if detail != "health" && detail != "inventory" {
		view.Foreground = "—"
		if group > 0 {
			view.Foreground = newProcessSnapshot().foregroundPrograms(group)
		}
	}
	return message{Type: "session", Owner: view.Owner, Action: view.State, Pinned: view.Pinned, PinUntil: view.PinUntil, Instance: view.Instance, Name: view.Name, SoftwareVersion: version,
		Attached: view.State == taskActive, PID: view.PID, AgeSeconds: view.Age, Foreground: view.Foreground, Directory: view.Directory}
}
func (tasks *daemonTasks) registerHostedShell(req hostedShellRequest) (*remoteShell, error) {
	s := &remoteShell{
		shellTask: shellTask{tasks: tasks, record: localShellRecord{Name: req.Name, Directory: req.Directory}, hosted: true,
			environment: req.Env, parent: req.parent, done: make(chan struct{})},
	}
	if err := tasks.register("", s); err != nil {
		if errors.Is(err, errObjectNameInUse) {
			return nil, fmt.Errorf("name %s already exists; use a different shell name", req.Name)
		}
		return nil, err
	}
	s.watchParent(req.parent)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		defer stop()
		s.watchLifetime(ctx)
	}()
	return s, nil
}

func (s *remoteShell) watchLifetime(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.terminal != nil {
			_ = s.terminal.Close()
		}
	}()
	// A failed initial attach should not leave an empty shell indefinitely.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	initialAttachDeadline := timer.C
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.endTerminal()
			s.finishLocked()
			s.mu.Unlock()
			return
		case <-s.done:
			return
		case <-initialAttachDeadline:
			s.mu.Lock()
			empty := s.terminal == nil
			if empty {
				s.endTerminal()
				s.finishLocked()
			}
			s.mu.Unlock()
			if empty {
				return
			}
			initialAttachDeadline = nil
		}
	}
}

func (s *remoteShell) hostedView(detail string) (shellView, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := shellView{
		Owner: s.parentIdentityLocked(), Pinned: s.pinned, PinUntil: s.pinUntil, Instance: s.record.UID,
		ID: "sh/" + s.record.Name, Name: s.record.Name, PID: s.process.PID,
	}
	view.State = s.state()
	if s.active != nil {
		view.SourceHost = s.active.sourceHost
	}
	if view.PID != 0 {
		age := int64(time.Since(s.started) / time.Second)
		view.Age = &age
	}
	group := 0
	if detail != "health" && s.terminal != nil {
		group, _ = s.terminal.ForegroundGroup()
	}
	return view, group
}

func (s *remoteShell) serveTask(c net.Conn, m message) error {
	s.handleTask(c, m)
	return nil
}

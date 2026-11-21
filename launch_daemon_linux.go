//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
)

// A launch is persistent. A spawn can be attached only after its single
// lifetime connection has claimed it. Transitions are serialized by remoteShell.mu.
type hostedLifetime uint8

const (
	persistentLaunch hostedLifetime = iota
	awaitingSpawnOwner
	ownedSpawn
)

type remoteShell struct {
	shellTask
	spawnRoutes map[string]*spawnRoute
	lifetime    hostedLifetime
}

func (s *remoteShell) start(m message) error {
	if s.record.UID == "" {
		s.record.UID = rand.Text()
	}
	env := s.shellEnvironment()
	path := localEnvironmentValue(env, "SHELL")
	if path == "" {
		path = "/bin/bash"
	}
	exe, _ := os.Executable()
	return s.startManagedShell(path, hostedShellEnvironment(env, m.Term, s.record.UID, exe), m)
}
func (s *remoteShell) attach(p *attachment, m message) error {
	s.mu.Lock()
	temporary := s.lifetime != persistentLaunch
	s.mu.Unlock()
	err := s.attachManaged(p, m, s.start, temporary)

	if errors.Is(err, errShellExited) {
		return fmt.Errorf("shell is exiting; use sshore launch HOST: to create a new one")
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
		saveShellTree(remoteTreeIdentity(s.record.UID), s.record.Name, nil, "detached")
		return s.record.Name, nil
	}, func(m message) error {
		switch m.Type {
		case "spawn_child":
			s.mu.Lock()
			req := &localDaemonRequest{Record: localShellRecord{Directory: s.record.Directory, Owner: s.process}, Env: append([]string(nil), s.environment...), Spawn: &spawnConfig{Local: true, Tree: shellTreeFor(remoteTreeIdentity(s.record.UID))}}
			s.mu.Unlock()
			go serveShortcutSpawn(p, s.done, func(c net.Conn) {
				s.requestSpawnRoute(c, message{SoftwareVersion: version, LocalRequest: req})
			})
		case "launch_child":
			s.mu.Lock()
			req := localDaemonRequest{Record: localShellRecord{Directory: s.record.Directory}, Env: append([]string(nil), s.environment...), Spawn: &spawnConfig{Local: true, Tree: shellTreeFor(remoteTreeIdentity(s.record.UID))}}
			s.mu.Unlock()
			go serveShortcutSpawn(p, s.done, func(c net.Conn) {
				if err := s.serveLocalLaunchRoute(c, p, req); err != nil {
					_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
				}
			})
		case "approval_ready":
			p.approvals.enable()
		case "approval_handoff", "approval_rejected":
			p.approvals.reply(m)
		case "session_targets":
			if err := p.controlMessage(s.sessionTargets(m)); err != nil {
				return err
			}
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
	if first.Type == "attach_control" {
		s.mu.Lock()
		unowned := s.lifetime == awaitingSpawnOwner
		s.mu.Unlock()
		if unowned {
			_ = writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: "spawn lifetime is not established"})
			return
		}
	}
	if handled, _ := s.serveCommand(c, first, s.serveControl, s.status); handled {
		return
	}
	switch first.Type {
	case "route_mount", "route_forward", "route_files":
		s.serveRoutedResource(c, first)
	case "shell_connect":
		_ = s.connectRouteShell(c, first)
	case "spawn_route_confirm":
		s.confirmSpawnRoute(c, first)
	case "spawn_route_request":
		s.requestSpawnRoute(c, first)
	case "spawn_route_connect":
		s.connectSpawnRoute(c, first)
	case "spawn_owner":
		s.serveSpawnOwner(c, first)
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
		view.Directory = cleanText(sessionDirectory(group, view.PID))
	}
	if detail != "health" && detail != "inventory" {
		view.Foreground = "—"
		if group > 0 {
			view.Foreground = newProcessSnapshot(proc.ReadProcesses()).foregroundPrograms(group)
		}
	}
	return message{Type: "session", Temporary: view.Temporary, Instance: view.Instance, Name: view.Name, SoftwareVersion: version,
		Attached: view.State == "attached", PID: view.PID, AgeSeconds: view.Age, Foreground: view.Foreground, Directory: view.Directory}
}
func (tasks *daemonTasks) registerHostedShell(req hostedShellRequest) error {
	s := &remoteShell{
		shellTask: shellTask{tasks: tasks, record: localShellRecord{UID: req.UID, Name: req.Name, Directory: req.Directory}, hosted: true,
			environment: req.Env, done: make(chan struct{})},
	}
	if req.Temporary {
		s.lifetime = awaitingSpawnOwner
	}
	if err := tasks.register(req.UID, s); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		defer stop()
		s.watchLifetime(ctx)
	}()
	return nil
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
	temporary := s.lifetime != persistentLaunch
	view := shellView{
		Lifetime: shellLifetime(temporary, true), Temporary: temporary, Instance: s.record.UID,
		ID: shellTaskName(s.record.Name, temporary), Name: s.record.Name, PID: s.process.PID,
	}
	view.State = s.state()
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

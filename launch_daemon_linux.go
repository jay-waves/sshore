//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/tmux"
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
	shellTerminal
	tasks       *daemonTasks
	spawnRoutes map[string]*spawnRoute
	lifetime    hostedLifetime
	environment []string
	directory   string
	killing     bool

	instance   string
	name       string
	exitReason string
	started    time.Time // shell lifetime, including time spent detached
	runtime    string
	done       chan struct{}
	once       sync.Once
}

func (s *remoteShell) stop() {
	s.once.Do(func() {
		saveShellTree(remoteTreeIdentity(s.instance), s.name, nil, "exited")
		close(s.done)
	})
}
func (s *remoteShell) start(m message) error {
	if s.instance == "" {
		s.instance = rand.Text()
	}
	env := s.environment
	if env == nil {
		env = os.Environ()
	}
	shellPath := localEnvironmentValue(env, "SHELL")
	if shellPath == "" {
		shellPath = "/bin/bash"
	}
	args, err := tmux.StartupArgs(shellPath, m.NoLogin, m.NoProfile)
	if err != nil {
		return err
	}
	env = append(append([]string(nil), env...), "TERM="+m.Term, "SSHORE_SOCKET="+s.instance, "SSHORE_TREE_ID="+remoteTreeIdentity(s.instance))
	if exe, err := os.Executable(); err == nil {
		env = append(env, "PATH="+filepath.Dir(exe)+":"+localEnvironmentValue(env, "PATH"))
	}
	err = s.startTerminal(shellPath, args, tmux.StartupEnv(shellPath, env, m.NoProfile), s.directory, m, func(err error, code int) {
		s.mu.Lock()
		p := s.lifecycle.End()
		if s.exitReason == "" {
			s.exitReason = fmt.Sprintf("shell exited (code %d)", code)
			if e, ok := errors.AsType[*exec.ExitError](err); ok {
				if status, ok := e.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					s.exitReason = "shell terminated by " + status.Signal().String()
				}
			}
		}
		reason := s.exitReason
		s.mu.Unlock()
		if p != nil {
			p.FinishExit(code, reason)
		}
		s.stop()
	})
	if err == nil {
		s.started = time.Now()
	}
	return err
}
func (s *remoteShell) attach(p *attachment, m message) (shellInput, error) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	if err := validateAttach(m); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.killing {
		return nil, fmt.Errorf("shell is being killed")
	}
	if m.Instance != "" && m.Instance != s.instance {
		return nil, fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	initial := s.terminal == nil
	if m.Action == "existing" && initial {
		return nil, fmt.Errorf("shell has not started; use sshore list HOST")
	}
	lease, err := s.attachTerminal(p, m, s.start, func() error {
		saveShellTreeWithProcess(remoteTreeIdentity(s.instance), s.name, m.Tree, "attached", m.OwnerProcess, shellLifetime(s.lifetime != persistentLaunch, true), m.SourceHost, m.SourceMachineID)
		return p.controlMessage(message{Type: "ready", Tree: shellTreeFor(remoteTreeIdentity(s.instance)), Temporary: s.lifetime != persistentLaunch, Instance: s.instance, PID: s.process.PID, Name: s.name})
	})
	if err != nil && initial && s.terminal != nil {
		s.lifecycle.End()
		s.exitReason = "initial attachment failed"
		s.terminal.Close()
		s.stop()
	}

	if errors.Is(err, tmux.ErrExited) {
		return nil, fmt.Errorf("shell is exiting; use sshore launch HOST: to create a new one")
	}
	if errors.Is(err, tmux.ErrAttached) {
		return nil, fmt.Errorf("shell already attached; detach the other connection first")
	}
	return lease, err
}

// Only the current attachment may stop the terminal's writer and hand it off.
func (s *remoteShell) clearAttachment(p *attachment, lease shellInput) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	detached, err := s.detachTerminal(p, lease)
	if err != nil {
		s.exitReason = "terminal input cancellation failed"
		s.stop()
	}
	if !detached {
		return "", false
	}
	saveShellTree(remoteTreeIdentity(s.instance), s.name, nil, "detached")
	return s.name, true
}
func (s *remoteShell) serveMux(c io.ReadWriteCloser) error {
	mux, control, terminal, m, err := acceptMux(c)
	if err != nil {
		return err
	}
	defer mux.Close()
	p := newAttachment(mux, control, terminal)
	lease, err := s.attach(p, m)
	if err != nil {
		_ = p.controlMessage(message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		_ = control.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = readMessage(control)
		return err
	}
	defer func() { p.Close(); s.clearAttachment(p, lease) }()
	go p.readInput(lease, &s.lifecycle)

	for {
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		switch m.Type {
		case "inventory":
			inv := localSessionInventory()
			for _, row := range inv.Shells {
				if err := p.controlMessage(message{Type: "inventory_row", Inventory: &inventory{Shells: []shellView{row}}}); err != nil {
					return err
				}
			}
			for _, row := range inv.Resources {
				if err := p.controlMessage(message{Type: "inventory_row", Inventory: &inventory{Resources: []resourceView{row}}}); err != nil {
					return err
				}
			}
			for _, row := range inv.Peers {
				if err := p.controlMessage(message{Type: "inventory_row", Inventory: &inventory{Peers: []peerView{row}}}); err != nil {
					return err
				}
			}
			if err := p.controlMessage(message{Type: "inventory_done", Inventory: &inventory{Errors: inv.Errors}}); err != nil {
				return err
			}
		case "spawn_child":
			s.mu.Lock()
			req := &localDaemonRequest{Record: localShellRecord{Directory: s.directory, Owner: s.process}, Env: append([]string(nil), s.environment...), Spawn: &spawnConfig{Local: true, Tree: shellTreeFor(remoteTreeIdentity(s.instance))}}
			s.mu.Unlock()
			go serveShortcutSpawn(p, s.done, func(c net.Conn) {
				s.requestSpawnRoute(c, message{SoftwareVersion: version, LocalRequest: req})
			})
		case "session_targets":
			if err := p.controlMessage(s.sessionTargets(m)); err != nil {
				return err
			}
		case "redraw":
			s.terminalMu.Lock()
			s.mu.Lock()
			if s.lifecycle.Active() == p && !p.Muted() {
				tmux.RequestRedraw(s.terminal, s.process.PID)
			}
			s.mu.Unlock()
			s.terminalMu.Unlock()
			if err := p.controlMessage(message{Type: "redrawn"}); err != nil {
				return err
			}
		case "resize":
			s.mu.Lock()
			var resizeErr error
			if s.lifecycle.Active() == p {
				resizeErr = s.terminal.Resize(tmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)})
			}
			s.mu.Unlock()
			if resizeErr != nil {
				return fmt.Errorf("resize terminal: %w", resizeErr)
			}
			if err := p.controlMessage(message{Type: "resized", Cols: m.Cols, Rows: m.Rows}); err != nil {
				return err
			}
		case "detach":
			if name, ok := s.clearAttachment(p, lease); ok {
				p.FinishDetach(name)
			}
			return nil
		default:
			return fmt.Errorf("unexpected control message: %s", m.Type)
		}
	}
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
	var err error
	_ = c.SetDeadline(time.Time{})
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
	case "spawn_transport":
		if checkPeerVersion(first.SoftwareVersion, "client") != nil {
			return
		}
		lease, err := dialHostedTask(context.Background(), s.instance, time.Second)
		if err != nil {
			return
		}
		defer lease.Close()
		_ = bridgeSpawnTransport(s.instance, lease, "", c)
	case "spawn_owner":
		s.serveSpawnOwner(c, first)
	case "mux":
		s.mu.Lock()
		unowned := s.lifetime == awaitingSpawnOwner
		s.mu.Unlock()
		if unowned {
			_ = writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: "spawn lifetime is not established"})
			return
		}
		if err := checkPeerVersion(first.SoftwareVersion, "client"); err != nil {
			_ = writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
			return
		}
		if writeMessage(c, message{Type: "mux_ready", SoftwareVersion: version}) == nil {
			_ = s.serveMux(c)
		}
	case "kill":
		if err := checkPeerVersion(first.SoftwareVersion, "client"); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
			return
		}
		if err := s.kill(first.Instance); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
			return
		}
		_ = writeMessage(c, message{Type: "killed"})
	case "approval_check":
		reply := message{Type: "approval_checked", SoftwareVersion: version}
		if err := s.checkApprovalSource(first); err != nil {
			reply.Type = "error"
			reply.Error = err.Error()
		}
		_ = writeMessage(c, reply)
	case "rename":
		if err := checkPeerVersion(first.SoftwareVersion, "client"); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
			return
		}
		if err := s.rename(first.Action, first.Name, first.Instance); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
			return
		}
		_ = writeMessage(c, message{Type: "renamed", Name: first.Name})
	case "status":
		s.mu.Lock()
		m := message{Type: "session", Temporary: s.lifetime != persistentLaunch, Instance: s.instance, Attached: s.lifecycle.Active() != nil, Name: s.name, SoftwareVersion: version}
		group := 0
		if s.terminal != nil {
			group, _ = s.terminal.ForegroundGroup()
		}
		if s.process.PID != 0 {
			m.PID = s.process.PID
			age := int64(time.Since(s.started) / time.Second)
			m.AgeSeconds = &age
		}
		s.mu.Unlock()
		if first.Action != "health" {
			if first.Action != "inventory" {
				m.Foreground = foregroundPrograms("/proc", group)
			}
			m.Directory = sessionDirectory(group, m.PID)
		}
		_ = writeMessage(c, m)
	case "detach":
		s.mu.Lock()
		p := s.lifecycle.Active()
		s.mu.Unlock()
		// Ask the attachment's control loop to handle finalization, avoiding two
		// concurrent readers of the control stream.
		if p != nil {
			if err := p.controlMessage(message{Type: "detach_requested"}); err != nil {
				_ = writeMessage(c, message{Type: "error", Error: "detach request could not reach the client; reconnect to check the shell"})
				return
			}
		}
		_ = writeMessage(c, message{Type: "detached"})
	case "approval_request":
		if err = s.requestApproval(c, first); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
		}
	default:
		_ = writeMessage(c, message{Type: "fatal", Error: "unsupported connection; update sshore on both computers"})
	}
}
func (tasks *daemonTasks) registerHostedShell(req hostedShellRequest) error {
	s := &remoteShell{
		tasks: tasks, instance: req.UID, name: req.Name, runtime: req.Runtime,
		environment: req.Env, directory: req.Directory, done: make(chan struct{}),
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
		if s.process.PID != 0 {
			reason := s.exitReason
			if reason == "" {
				reason = "daemon stopped (signal or shutdown)"
			}
			saveSessionExit(filepath.Join(s.runtime, s.instance+".sock"), s.name, reason)
		}
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
			s.lifecycle.End()
			s.stop()
			s.mu.Unlock()
			return
		case <-s.done:
			return
		case <-initialAttachDeadline:
			s.mu.Lock()
			empty := s.terminal == nil
			if empty {
				s.lifecycle.End()
				s.stop()
			}
			s.mu.Unlock()
			if empty {
				return
			}
			initialAttachDeadline = nil
		}
	}
}

func (s *remoteShell) kill(instance string) error {
	return s.killWithReason(instance, "killed by sshore kill")
}

func (s *remoteShell) killWithReason(instance, reason string) error {
	s.mu.Lock()
	if instance == "" || instance != s.instance {
		s.mu.Unlock()
		return fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	if s.process.PID == 0 {
		s.lifecycle.End()
		s.stop()
		s.mu.Unlock()
		return nil
	}
	s.killing = true
	pid, start := s.process.PID, s.process.Start
	if s.exitReason == "" {
		s.exitReason = reason
	}
	s.mu.Unlock()
	if err := stopOwnedProcesses([]setupProcess{{PID: pid, Start: start}}); err != nil {
		s.mu.Lock()
		s.killing = false
		s.mu.Unlock()
		return err
	}
	s.stop()
	return nil
}

func (s *remoteShell) taskName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}
func (s *remoteShell) expired(time.Time) bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
func (s *remoteShell) serveTask(c net.Conn, m message) error {
	s.handleTask(c, m)
	return nil
}

func (s *remoteShell) stopTask() error { return s.kill(s.instance) }

func (s *remoteShell) runtimeDirectory() string { return s.runtime }

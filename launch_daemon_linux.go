//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
	"github.com/jay-waves/sshore/internal/pty"
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
	args, err := pty.StartupArgs(shellPath, m.NoLogin, m.NoProfile)
	if err != nil {
		return err
	}
	env = append(append([]string(nil), env...), "TERM="+m.Term, "SSHORE_SOCKET="+s.instance, "SSHORE_TREE_ID="+remoteTreeIdentity(s.instance))
	if exe, err := os.Executable(); err == nil {
		env = append(env, "PATH="+filepath.Dir(exe)+":"+localEnvironmentValue(env, "PATH"))
	}
	err = s.startTerminal(shellPath, args, pty.StartupEnv(shellPath, env, m.NoProfile), s.directory, m, func(err error, code int) {
		s.mu.Lock()
		p := s.endTerminal()
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
func (s *remoteShell) attach(p *attachment, m message) error {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	if err := validateAttach(m); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.killing {
		return fmt.Errorf("shell is being killed")
	}
	if m.Instance != "" && m.Instance != s.instance {
		return fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	initial := s.terminal == nil
	if m.Action == "existing" && initial {
		return fmt.Errorf("shell has not started; use sshore list HOST")
	}
	err := s.attachTerminal(p, m, s.start, func() error {
		saveShellTreeWithProcess(remoteTreeIdentity(s.instance), s.name, m.Tree, "attached", m.OwnerProcess, shellLifetime(s.lifetime != persistentLaunch, true), m.SourceHost, m.SourceMachineID)
		return p.controlMessage(message{Type: "ready", Tree: shellTreeFor(remoteTreeIdentity(s.instance)), Temporary: s.lifetime != persistentLaunch, Instance: s.instance, PID: s.process.PID, Name: s.name})
	})
	if err != nil && initial && s.terminal != nil {
		s.endTerminal()
		s.exitReason = "initial attachment failed"
		s.terminal.Close()
		s.stop()
	}

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
			s.stop()
			return "", nil
		}
		saveShellTree(remoteTreeIdentity(s.instance), s.name, nil, "detached")
		return s.name, nil
	}, func(m message) error {
		switch m.Type {
		case "inventory":
			inv := collectManagedInventory(false, false)
			inv.collectPeers()
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
		case "approval_ready":
			p.approvals.enable()
		case "queued", "approval_rejected":
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
	var err error
	_ = c.SetDeadline(time.Time{})
	switch first.Type {
	case "terminal_open":
		_ = s.endpoints.serve(c, first)
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
	case "attach_control":
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
		if writeMessage(c, message{Type: "attach_control_ready", SoftwareVersion: version}) == nil {
			_ = s.serveControl(c)
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
		view, group := s.view(first.Action)
		if first.Action != "health" && first.Action != "inventory" {
			view.Foreground = "—"
			if group > 0 {
				view.Foreground = newProcessSnapshot(proc.ReadProcesses()).foregroundPrograms(group)
			}
		}
		_ = writeMessage(c, message{
			Type: "session", Temporary: view.Temporary, Instance: view.Instance, Name: view.Name, SoftwareVersion: version,
			Attached: view.State == "attached", PID: view.PID, AgeSeconds: view.Age, Foreground: view.Foreground, Directory: view.Directory,
		})
	case "detach":
		s.mu.Lock()
		p := s.active
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
			s.endTerminal()
			s.stop()
			s.mu.Unlock()
			return
		case <-s.done:
			return
		case <-initialAttachDeadline:
			s.mu.Lock()
			empty := s.terminal == nil
			if empty {
				s.endTerminal()
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
		s.endTerminal()
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

func (s *remoteShell) view(detail string) (shellView, int) {
	s.mu.Lock()
	temporary := s.lifetime != persistentLaunch
	view := shellView{
		Lifetime: shellLifetime(temporary, true), Temporary: temporary, Instance: s.instance,
		ID: shellTaskName(s.name, temporary), Name: s.name, PID: s.process.PID,
	}
	switch {
	case s.exited:
		view.State = "unavailable"
	case s.active != nil:
		view.State = "attached"
	case view.PID == 0:
		view.State = "starting"
	default:
		view.State = "detached"
	}
	if view.PID != 0 {
		age := int64(time.Since(s.started) / time.Second)
		view.Age = &age
	}
	group := 0
	if detail != "health" && s.terminal != nil {
		group, _ = s.terminal.ForegroundGroup()
	}
	s.mu.Unlock()

	if detail != "health" {
		view.Directory = cleanText(sessionDirectory(group, view.PID))
	}
	return view, group
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

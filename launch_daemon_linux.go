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

// Each attachment owns one mux session. Local helpers use Unix sockets for
// approval, forwarding control and detach requests. Approved file bytes flow
// directly on client-initiated streams, without a helper process or PTY relay.

type remoteShell struct {
	tasks           *daemonTasks
	spawnRoutes     map[string]*spawnRoute
	temporary       bool
	spawnOwned      bool
	environment     []string
	directory       string
	processIdentity string
	killing         bool

	instance   string
	name       string
	exitReason string
	terminalMu sync.Mutex // serializes state snapshots with PTY output
	mouse      tmux.InputState
	mu         sync.Mutex
	lifecycle  tmux.Lifecycle[*attachment]
	started    time.Time // shell lifetime, including time spent detached
	master     *tmux.Terminal
	cmd        *exec.Cmd
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
func (s *remoteShell) start(cols, rows uint16, term string, startup ...bool) error {
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
	args, err := tmux.StartupArgs(shellPath, len(startup) > 0 && startup[0], len(startup) > 1 && startup[1])
	if err != nil {
		return err
	}
	cmd := exec.Command(shellPath, args...)
	cmd.Dir = s.directory
	cmd.Env = append(append([]string(nil), env...), "TERM="+term, "SSHORE_SOCKET="+s.instance, "SSHORE_TREE_ID="+remoteTreeIdentity(s.instance))
	if exe, err := os.Executable(); err == nil {
		cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(exe)+":"+localEnvironmentValue(env, "PATH"))
	}
	cmd.Env = tmux.StartupEnv(shellPath, cmd.Env, len(startup) > 1 && startup[1])
	master, err := tmux.StartCommand(cmd, tmux.Size{Cols: max(cols, 1), Rows: max(rows, 1)})
	if err != nil {
		return err
	}
	s.cmd, s.master = cmd, master
	s.processIdentity, err = processStart(cmd.Process.Pid)
	if err != nil {
		master.Close()
		return err
	}
	s.started = time.Now()
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				s.terminalMu.Lock()
				s.mouse.Observe(buf[:n])
				s.mu.Lock()
				p := s.lifecycle.Active()
				s.mu.Unlock()
				if p != nil {
					if p.output(message{Type: "output", Data: buf[:n]}) != nil {
						p.Close()
					}
				}
				s.terminalMu.Unlock()
			}
			if err != nil {
				break
			}
		}
		err := master.Wait()
		code := 0
		if err != nil {
			code = 1
			if e, ok := errors.AsType[*exec.ExitError](err); ok {
				code = e.ExitCode()
			}
		}
		s.mu.Lock()
		p := s.lifecycle.End()
		if s.exitReason == "" {
			s.exitReason = fmt.Sprintf("shell exited (code %d)", code)
			if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				s.exitReason = "shell terminated by " + status.Signal().String()
			}
		}
		reason := s.exitReason
		s.mu.Unlock()
		if p != nil {
			p.FinishExit(code, reason)
		}
		s.stop()
	}()
	return nil
}
func (s *remoteShell) attach(p *attachment, m message) error {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	if err := validateAttach(m); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	startedHere := false
	err := s.lifecycle.Attach(p, func() error {
		if s.killing {
			return fmt.Errorf("shell is being killed")
		}
		if m.Instance != "" && m.Instance != s.instance {
			return fmt.Errorf("shell instance changed; refresh with sshore list")
		}
		if m.Action == "existing" && s.cmd == nil {
			return fmt.Errorf("shell has not started; use sshore list HOST")
		}
		if (m.NoLogin || m.NoProfile) && s.master != nil {
			return fmt.Errorf("startup options require a new managed shell")
		}
		reconnecting := s.master != nil
		if s.master == nil {
			if err := s.start(m.Cols, m.Rows, m.Term, m.NoLogin, m.NoProfile); err != nil {
				return err
			}
			startedHere = true
		}
		if err := s.master.Resize(tmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}); err != nil {
			return err
		}
		saveShellTreeWithProcess(remoteTreeIdentity(s.instance), s.name, m.Tree, "attached", m.OwnerProcess, shellLifetime(s.temporary, true), m.SourceHost, m.SourceMachineID)
		if err := p.controlMessage(message{Type: "ready", Tree: shellTreeFor(remoteTreeIdentity(s.instance)), Temporary: s.temporary, Instance: s.instance, PID: s.cmd.Process.Pid, Name: s.name}); err != nil {
			return err
		}
		if !p.Muted() {
			if data := s.mouse.Replay(); len(data) > 0 {
				if err := p.output(message{Type: "output", Data: data}); err != nil {
					return err
				}
			}
		}
		p.SetInput(tmux.NewInputLease(s.master))
		if reconnecting && !p.Muted() {
			tmux.RequestRedraw(s.master, s.cmd.Process.Pid)
		}
		return nil
	})
	if err != nil && startedHere {
		s.lifecycle.End()
		s.exitReason = "initial attachment failed"
		_ = s.master.Close()
		s.stop()
	}
	if errors.Is(err, tmux.ErrExited) {
		return fmt.Errorf("shell is exiting; use sshore launch HOST to create a new one")
	}
	if errors.Is(err, tmux.ErrAttached) {
		return fmt.Errorf("shell already attached; detach the other connection first")
	}
	return err
}
func (s *remoteShell) release(p *attachment) {
	p.Close()
	// Stop this attachment's writer before allowing another client to attach.
	s.clearAttachment(p)
}

func (s *remoteShell) clearAttachment(p *attachment) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle.Active() == p && p.Input() != nil {
		if err := p.Input().Stop(); err != nil {
			s.lifecycle.Release(p)
			s.exitReason = "terminal input cancellation failed"
			s.stop()
			return false
		}
	}
	detached := s.lifecycle.Detach(p)
	if detached {
		saveShellTree(remoteTreeIdentity(s.instance), s.name, nil, "detached")
	}
	return detached
}
func (s *remoteShell) detach(p *attachment) {
	if !s.clearAttachment(p) {
		p.Close()
		return
	}
	s.mu.Lock()
	name := s.name
	s.mu.Unlock()
	tmux.FinishDetach(p.ControlStream(), func() error { return p.controlMessage(message{Type: "detached", Name: name}) }, p.Close)
}
func (s *remoteShell) serveMux(c io.ReadWriteCloser) error {
	mux, control, terminal, m, err := acceptMux(c)
	if err != nil {
		return err
	}
	defer mux.Close()
	p := newAttachment(mux, control, terminal)
	defer s.release(p)
	if err = s.attach(p, m); err != nil {
		_ = p.controlMessage(message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		_ = control.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = readMessage(control)
		return err
	}
	go p.readInput(&s.lifecycle)

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
			req := &localDaemonRequest{Record: localShellRecord{Directory: s.directory, Owner: spawnOwner{PID: s.cmd.Process.Pid, Start: s.processIdentity}}, Env: append([]string(nil), s.environment...), Spawn: &spawnConfig{Local: true, Tree: shellTreeFor(remoteTreeIdentity(s.instance))}}
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
				tmux.RequestRedraw(s.master, s.cmd.Process.Pid)
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
				resizeErr = s.master.Resize(tmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)})
			}
			s.mu.Unlock()
			if resizeErr != nil {
				return fmt.Errorf("resize terminal: %w", resizeErr)
			}
			if err := p.controlMessage(message{Type: "resized", Cols: m.Cols, Rows: m.Rows}); err != nil {
				return err
			}
		case "detach":
			s.detach(p)
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
		unowned := s.temporary && !s.spawnOwned
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
		m := message{Type: "session", Temporary: s.temporary, Instance: s.instance, Attached: s.lifecycle.Active() != nil, Name: s.name, SoftwareVersion: version}
		group := 0
		if s.master != nil {
			group, _ = s.master.ForegroundGroup()
		}
		if s.cmd != nil {
			m.PID = s.cmd.Process.Pid
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
func serveRemoteShellAt(tasks *daemonTasks, runtime, nameLabel, uid string, env []string, directory string, ready func() error, temporary ...bool) error {
	if !validTaskUID(uid) {
		return fmt.Errorf("invalid shell identity")
	}
	s := &remoteShell{tasks: tasks, temporary: len(temporary) > 0 && temporary[0], instance: uid, environment: env, directory: directory, runtime: runtime, name: nameLabel, done: make(chan struct{})}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cmd != nil {
			reason := s.exitReason
			if reason == "" {
				reason = "daemon stopped (signal or shutdown)"
			}
			saveSessionExit(filepath.Join(s.runtime, s.instance+".sock"), s.name, reason)
		}
		if s.master != nil {
			_ = s.master.Close()
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		select {
		case <-ctx.Done():
			s.stop()
		case <-s.done:
		}
	}()
	if err := tasks.register(uid, s); err != nil {
		s.stop()
		return err
	}
	if ready != nil {
		if err := ready(); err != nil {
			s.stop()
			return err
		}
	}
	// A failed initial attach should not leave an empty daemon indefinitely.
	go func() {
		select {
		case <-time.After(30 * time.Second):
			s.mu.Lock()
			empty := s.master == nil
			s.mu.Unlock()
			if empty {
				s.stop()
			}
		case <-s.done:
		}
	}()
	<-s.done
	return nil
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
	if s.cmd == nil {
		s.lifecycle.End()
		s.stop()
		s.mu.Unlock()
		return nil
	}
	s.killing = true
	pid, start := s.cmd.Process.Pid, s.processIdentity
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

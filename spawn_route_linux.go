//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	forwardio "github.com/jay-waves/sshore/internal/forward"
	"github.com/jay-waves/sshore/internal/proc"
	"golang.org/x/term"
)

type spawnRoute struct {
	open         taskEndpoint
	drain        context.Context
	streams      sync.WaitGroup
	confirmation chan error // consumed once under the shell lock
}

// Open an authenticated task endpoint within the scope of the parent capability.
func (s *remoteShell) connectSpawnRoute(c net.Conn, request message) {
	if checkPeerVersion(request.SoftwareVersion, "client") != nil || request.Instance != s.record.UID {
		_ = writeMessage(c, message{Type: "error", Error: "parent shell instance changed"})
		return
	}
	s.mu.Lock()
	route := s.spawnRoutes[request.Token]
	if route != nil {
		route.streams.Add(1)
	}
	s.mu.Unlock()
	if route == nil {
		_ = writeMessage(c, message{Type: "error", Error: "spawn route is unavailable"})
		return
	}
	defer route.streams.Done()
	stop := context.AfterFunc(route.drain, func() { c.Close() })
	defer stop()
	stream, err := route.open()
	if err != nil {
		_ = writeMessage(c, message{Type: "error", Error: "spawn route is unavailable"})
		return
	}
	stopTarget := context.AfterFunc(route.drain, func() { stream.Close() })
	defer stopTarget()
	if writeMessage(c, message{Type: "route_connected", SoftwareVersion: version}) != nil {
		_ = stream.Close()
		return
	}
	forwardio.Bridge(c, stream)
}

func (s *remoteShell) requestSpawnRoute(c net.Conn, request message) {
	req := request.LocalRequest
	if checkPeerVersion(request.SoftwareVersion, "launcher") != nil || req == nil || req.Spawn == nil || !filepath.IsAbs(req.Record.Directory) {
		return
	}
	identity, err := proc.LiveIdentity(req.Record.Owner.PID)
	if err != nil || req.Record.Owner.PID <= 1 || identity != req.Record.Owner.Start {
		_ = writeMessage(c, message{Type: "fatal", Error: "spawn owner exited"})
		return
	}
	s.mu.Lock()
	parent := s.active
	s.mu.Unlock()
	if parent == nil {
		_ = writeMessage(c, message{Type: "fatal", Error: "managed attachment required"})
		return
	}
	if req.Spawn.Local {
		open, instance, done, stop, err := s.startLocalSpawn(*req)
		if err == nil {
			defer stop()
			ready := message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: instance}
			err = s.serveSpawnRoute(c, parent, req.Spawn, open, ready, done, false)
		}
		if err != nil {
			_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
		}
		return
	}
	// withRemoteSpawn reports setup and attachment failures on c.
	_ = withRemoteSpawn(c, *req, func(open taskEndpoint, ready message, done <-chan struct{}) error {
		return s.serveSpawnRoute(c, parent, req.Spawn, open, ready, done, false)
	})
}

func (s *remoteShell) serveSpawnRoute(c net.Conn, parent *attachment, spawn *spawnConfig, open taskEndpoint, ready message, ownerDone <-chan struct{}, persistent bool) error {
	drain, cancel := context.WithCancel(context.Background())
	defer cancel()
	token := resourceToken()
	confirmed := make(chan error, 1)
	route := &spawnRoute{open: whileSpawnOwned(ownerDone, open), drain: drain, confirmation: confirmed}
	s.mu.Lock()
	if s.active != parent {
		s.mu.Unlock()
		return fmt.Errorf("parent attachment changed")
	}
	if s.spawnRoutes == nil {
		s.spawnRoutes = make(map[string]*spawnRoute)
	}
	s.spawnRoutes[token] = route
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.spawnRoutes, token)
		s.mu.Unlock()
		drained := make(chan struct{})
		go func() {
			route.streams.Wait()
			close(drained)
		}()
		if spawnEnded(ownerDone) {
			timer := time.NewTimer(spawnDrainTimeout)
			defer timer.Stop()
			select {
			case <-drained:
				return
			case <-timer.C:
			}
		}
		cancel()
		<-drained
	}()
	if err := writeMessage(c, ready); err != nil {
		return err
	}
	action := "remote"
	if spawn.Local {
		action = "local"
		if persistent {
			action = "local-launch"
		}
	}
	if err := parent.controlMessage(message{Type: "spawn_route", Action: action, Token: token, Name: spawn.Name, SourceHost: spawn.Host, NoLogin: spawn.NoLogin, NoProfile: spawn.NoProfile, Tree: spawn.Tree}); err != nil {
		return err
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case err := <-confirmed:
		if err != nil {
			return err
		}
	case <-s.done:
		return fmt.Errorf("parent shell exited")
	case <-ownerDone:
		return fmt.Errorf("spawn owner ended before attachment")
	case <-timer.C:
		return fmt.Errorf("spawn attachment timed out")
	}
	if err := writeMessage(c, message{Type: "handoff_accepted"}); err != nil {
		return err
	}
	_ = c.Close()
	<-ownerDone
	return nil
}

func handoffRemoteSpawn(o options) (bool, error) {
	socket := os.Getenv("SSHORE_SOCKET")
	if socket == "" || o.command != "spawn" || o.detached || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return false, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return true, err
	}
	env := os.Environ()
	if !o.local {
		executable, err := exec.LookPath(sshExecutable())
		if err != nil {
			return true, err
		}
		executable, err = filepath.Abs(executable)
		if err != nil {
			return true, err
		}
		env = append(env, "SSHORE_SSH="+executable)
	}
	req := &localDaemonRequest{Record: localShellRecord{Owner: o.owner, Directory: cwd}, Env: env, Spawn: &spawnConfig{NoLogin: o.noLogin, NoProfile: o.noProfile, Tree: currentShellTree(), Local: o.local, Host: o.host, Name: o.session, Server: o.server, SSH: o.ssh}}
	if !o.local {
		auth, stop, err := startSSHAuthBroker()
		if err != nil {
			return true, err
		}
		defer stop()
		req.Auth = auth
	}
	c, err := dialHostedTask(context.Background(), socket, requestTimeout)
	if err != nil {
		return true, err
	}
	defer c.Close()
	if err := writeMessage(c, message{Type: "spawn_route_request", SoftwareVersion: version, LocalRequest: req}); err != nil {
		return true, err
	}
	if _, err := negotiateSetup(c, c, o.host, false); err != nil {
		return true, err
	}
	reply, err := readMessage(c)
	if err != nil {
		return true, err
	}
	if reply.Type != "handoff_accepted" {
		return true, fmt.Errorf("spawn handoff: %s", reply.Error)
	}
	return true, nil
}

func (s *remoteShell) confirmSpawnRoute(c net.Conn, request message) {
	if checkPeerVersion(request.SoftwareVersion, "client") != nil || request.Instance != s.record.UID {
		_ = writeMessage(c, message{Type: "error", Error: "parent shell instance changed"})
		return
	}
	s.mu.Lock()
	route := s.spawnRoutes[request.Token]
	if route == nil {
		s.mu.Unlock()
		_ = writeMessage(c, message{Type: "error", Error: "spawn route expired"})
		return
	}
	if route.confirmation == nil {
		s.mu.Unlock()
		_ = writeMessage(c, message{Type: "error", Error: "spawn route confirmation already received"})
		return
	}
	var err error
	if request.Error != "" {
		err = errors.New(request.Error)
	}
	route.confirmation <- err
	route.confirmation = nil
	s.mu.Unlock()
	_ = writeMessage(c, message{Type: "route_confirmed"})
}

func (s *remoteShell) createRouteShell(req localDaemonRequest, temporary bool) (string, error) {
	selected, err := selectHostedShell(req.Spawn.Name, "new")
	if err != nil {
		return "", err
	}
	launch := hostedShellRequest{Directory: req.Record.Directory, Name: selected.name, UID: selected.uid, Env: req.Env, Temporary: temporary}
	if err := s.tasks.registerHostedShell(launch); err != nil {
		return "", err
	}
	return selected.uid, nil
}

// The route is temporary, but the target has its own persistent lifetime.
func (s *remoteShell) serveLocalLaunchRoute(c net.Conn, parent *attachment, req localDaemonRequest) error {
	instance, err := s.createRouteShell(req, false)
	if err != nil {
		return err
	}
	child, ok := s.tasks.lookup(instance).(*remoteShell)
	if !ok {
		return fmt.Errorf("new launch is unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.done:
		case <-child.done:
		case <-ctx.Done():
		}
		cancel()
	}()
	open := func() (net.Conn, error) { return openHostedEndpoint(ctx, instance) }
	ready := message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: instance}
	return s.serveSpawnRoute(c, parent, req.Spawn, open, ready, ctx.Done(), true)
}

func (s *remoteShell) startLocalSpawn(req localDaemonRequest) (taskEndpoint, string, <-chan struct{}, func(), error) {
	instance, err := s.createRouteShell(req, true)
	if err != nil {
		return nil, "", nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopWatching := superviseSpawnOwner(req.Record.Owner, cancel)
	open := func() (net.Conn, error) { return openHostedEndpoint(ctx, instance) }
	stopOwner, done, err := claimSpawnOwner(ctx, open, instance)
	if err != nil {
		cancel()
		stopWatching()
		return nil, "", nil, nil, err
	}
	stop := func() {
		stopOwner()
		cancel()
		stopWatching()
	}
	return open, instance, done, stop, nil
}

func openHostedEndpoint(ctx context.Context, instance string) (net.Conn, error) {
	c, err := dialHostedTask(ctx, instance, time.Second)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// Sibling selection stays on this machine; names never become root SSH aliases.
func (s *remoteShell) connectRouteShell(c net.Conn, request message) (err error) {
	defer func() {
		if err != nil {
			_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
		}
	}()
	if err = checkPeerVersion(request.SoftwareVersion, "client"); err != nil {
		return err
	}
	s.mu.Lock()
	if s.process.PID == 0 {
		s.mu.Unlock()
		return fmt.Errorf("managed shell required")
	}
	req := localDaemonRequest{Record: localShellRecord{Directory: s.record.Directory, Owner: s.process}, Env: append([]string(nil), s.environment...), Spawn: &spawnConfig{Local: true, Name: request.Name}}
	s.mu.Unlock()

	var instance string
	switch request.Action {
	case "attach":
		instance, err = resolveSession(request.Name, request.Instance)
		if err != nil {
			return err
		}
		if instance == "" {
			return fmt.Errorf("shell is unavailable")
		}
	case "launch":
		instance, err = s.createRouteShell(req, false)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid shell selection")
	}
	target, err := openHostedEndpoint(context.Background(), instance)
	if err != nil {
		return err
	}
	defer target.Close()
	if err := writeMessage(c, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: instance}); err != nil {
		return err
	}
	forwardio.Bridge(c, target)
	return nil
}

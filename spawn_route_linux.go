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
	"time"

	"github.com/jay-waves/sshore/internal/wire"
	"github.com/xtaci/smux"
	"golang.org/x/term"
)

type spawnRoute struct {
	mux          *smux.Session
	confirmation chan error // consumed once under the shell lock
}

// The route exposes only attachment streams, never the initiating environment.
func (s *remoteShell) connectSpawnRoute(c net.Conn, request message) {
	if checkPeerVersion(request.SoftwareVersion, "client") != nil || request.Instance != s.instance {
		_ = writeMessage(c, message{Type: "error", Error: "parent shell instance changed"})
		return
	}
	s.mu.Lock()
	route := s.spawnRoutes[request.Token]
	s.mu.Unlock()
	if route == nil {
		_ = writeMessage(c, message{Type: "error", Error: "spawn route is unavailable"})
		return
	}
	stream, err := route.mux.OpenStream()
	if err != nil {
		_ = writeMessage(c, message{Type: "error", Error: "spawn route is unavailable"})
		return
	}
	if writeMessage(c, message{Type: "route_connected", SoftwareVersion: version}) != nil {
		_ = stream.Close()
		return
	}
	wire.Bridge(c, stream)
}

func (s *remoteShell) requestSpawnRoute(c net.Conn, request message) {
	req := request.LocalRequest
	if checkPeerVersion(request.SoftwareVersion, "launcher") != nil || req == nil || req.Spawn == nil || !filepath.IsAbs(req.Record.Directory) {
		return
	}
	identity, err := spawnProcessIdentity(req.Record.Owner.PID)
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
	runConnection := runSpawnConnection
	if req.Spawn.Local {
		runConnection = s.runLocalSpawnRoute
	}
	err = runConnection(c, *req, func(mx *smux.Session) error {
		token := resourceToken()
		confirmed := make(chan error, 1)
		route := &spawnRoute{mux: mx, confirmation: confirmed}
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
		defer func() { s.mu.Lock(); delete(s.spawnRoutes, token); s.mu.Unlock() }()
		action := "remote"
		if req.Spawn.Local {
			action = "local"
		}
		if err := parent.controlMessage(message{Type: "spawn_route", Action: action, Token: token, Name: req.Spawn.Name, SourceHost: req.Spawn.Host, NoLogin: req.Spawn.NoLogin, NoProfile: req.Spawn.NoProfile, Tree: req.Spawn.Tree}); err != nil {
			return err
		}
		select {
		case err := <-confirmed:
			if err != nil {
				return err
			}
		case <-s.done:
			return fmt.Errorf("parent shell exited")
		case <-mx.CloseChan():
			return fmt.Errorf("spawn transport closed before attachment")
		case <-time.After(15 * time.Second):
			return fmt.Errorf("spawn attachment timed out")
		}
		if err := writeMessage(c, message{Type: "handoff_accepted"}); err != nil {
			return err
		}
		<-mx.CloseChan()
		return nil
	})
	if err != nil {
		_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
	}
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
	if checkPeerVersion(request.SoftwareVersion, "client") != nil || request.Instance != s.instance {
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

func (s *remoteShell) createRouteShell(req localDaemonRequest, temporary bool) (net.Conn, error) {
	selected, err := selectHostedShell(req.Spawn.Name, "new")
	if err != nil {
		return nil, err
	}
	runtime, err := runtimeDir()
	if err != nil {
		return nil, err
	}
	launch := hostedShellRequest{Runtime: runtime, Directory: req.Record.Directory, Name: selected.name, UID: selected.uid, Env: req.Env, Temporary: temporary}
	if err := s.tasks.registerHostedShell(launch); err != nil {
		return nil, err
	}
	return dialHostedTask(context.Background(), selected.uid, time.Second)
}

func (s *remoteShell) runLocalSpawnRoute(c net.Conn, req localDaemonRequest, attached func(*smux.Session) error) error {
	transport, err := s.createRouteShell(req, true)
	if err != nil {
		return err
	}
	defer transport.Close()
	if err := writeMessage(transport, message{Type: "spawn_transport", SoftwareVersion: version}); err != nil {
		return err
	}
	mx, err := clientMux(transport)
	if err != nil {
		return err
	}
	defer mx.Close()
	_ = transport.SetDeadline(time.Time{})
	stopWatching := superviseSpawnOwner(req.Record.Owner, func() { _ = mx.Close() })
	defer stopWatching()
	if err := writeMessage(c, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed"}); err != nil {
		return err
	}
	return attached(mx)
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
	req := localDaemonRequest{Record: localShellRecord{Directory: s.directory, Owner: s.process}, Env: append([]string(nil), s.environment...), Spawn: &spawnConfig{Local: true, Name: request.Name}}
	s.mu.Unlock()
	if request.Action == "spawn" {
		return s.runLocalSpawnRoute(c, req, func(mx *smux.Session) error {
			stream, err := mx.OpenStream()
			if err != nil {
				return err
			}
			wire.Bridge(c, stream)
			<-mx.CloseChan()
			return nil
		})
	}
	var target net.Conn
	switch request.Action {
	case "attach":
		if err := validSession(request.Name); err != nil {
			return err
		}
		path, err := resolveSession(request.Name, request.Instance)
		if err != nil {
			return err
		}
		if path == "" {
			return fmt.Errorf("shell is unavailable")
		}
		target, err = dialHostedTask(context.Background(), path, time.Second)
		if err != nil {
			return err
		}
	case "launch":
		target, err = s.createRouteShell(req, false)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid shell selection")
	}
	defer target.Close()
	if err := writeMessage(c, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed"}); err != nil {
		return err
	}
	_ = target.SetDeadline(time.Time{})
	wire.Bridge(c, target)
	return nil
}

//go:build linux

package main

import (
	"context"
	"fmt"
	"golang.org/x/term"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/xtaci/smux"
)

type spawnRoute struct {
	mux       *smux.Session
	connected chan error
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
		return
	}
	defer stream.Close()
	if writeMessage(c, message{Type: "route_connected", SoftwareVersion: version}) != nil {
		return
	}
	proxySpawn(c, stream)
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
	parent := s.lifecycle.Active()
	recovery := s.plain
	s.mu.Unlock()
	if parent == nil || recovery {
		_ = writeMessage(c, message{Type: "fatal", Error: "managed attachment required"})
		return
	}
	runConnection := runSpawnConnection
	if req.Spawn.Local {
		runConnection = runLocalSpawnRoute
	}
	err = runConnection(c, *req, func(mx *smux.Session) error {
		token := resourceToken()
		route := &spawnRoute{mux: mx, connected: make(chan error, 1)}
		s.mu.Lock()
		if s.lifecycle.Active() != parent {
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
		case err := <-route.connected:
			if err != nil {
				return err
			}
		case <-s.done:
			return fmt.Errorf("parent shell exited")
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
	auth, stop, err := startSpawnAuth()
	if err != nil {
		return true, err
	}
	defer stop()
	env = append(env, auth...)
	req := &localDaemonRequest{Record: localShellRecord{Owner: o.owner, Directory: cwd}, Env: env, Spawn: &spawnConfig{NoLogin: o.noLogin, NoProfile: o.noProfile, Tree: currentShellTree(), Local: o.local, Host: o.host, Name: o.session, Server: o.server, SSH: o.ssh}}
	c, err := dialHostedTask(context.Background(), socket, requestTimeout)
	if err != nil {
		return true, err
	}
	defer c.Close()
	if err := writeMessage(c, message{Type: "spawn_route_request", SoftwareVersion: version, LocalRequest: req}); err != nil {
		return true, err
	}
	if err := negotiateSetup(c, c, &o); err != nil {
		return true, err
	}
	if o.plain {
		return true, fmt.Errorf("nested spawn requires managed setup; reconnect for recovery")
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
	s.mu.Unlock()
	if route == nil {
		_ = writeMessage(c, message{Type: "error", Error: "spawn route expired"})
		return
	}
	var err error
	if request.Error != "" {
		err = fmt.Errorf("%s", request.Error)
	}
	select {
	case route.connected <- err:
	default:
	}
	_ = writeMessage(c, message{Type: "route_confirmed"})
}

func createRouteShell(req localDaemonRequest, temporary bool) (net.Conn, error) {
	names := []string{req.Spawn.Name}
	fresh := req.Spawn.Name == ""
	if fresh {
		names = automaticSessionNames()
	}
	reservation, err := reserveSession(names, fresh)
	if err != nil {
		return nil, err
	}

	if reservation.conn != nil {
		return nil, fmt.Errorf("shell already exists")
	}

	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	label := reservation.publicName
	if label == "" {
		label = reservation.name
	}
	if err := createRemoteShellUsing(executable, reservation.name, label, reservation.namespace.uid, temporary, req.Record.Directory, req.Env); err != nil {
		return nil, err
	}
	transport, err := dialHostedTask(context.Background(), reservation.name, time.Second)
	if err != nil {
		return nil, err
	}
	return transport, nil
}

func runLocalSpawnRoute(c net.Conn, req localDaemonRequest, attached func(*smux.Session) error) error {
	transport, err := createRouteShell(req, true)
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
	done := make(chan struct{})
	defer close(done)
	go watchSpawnOwner(req.Record.Owner, done, func() { mx.Close(); transport.Close() })
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
	if s.plain || s.cmd == nil {
		s.mu.Unlock()
		return fmt.Errorf("managed shell required")
	}
	req := localDaemonRequest{Record: localShellRecord{Directory: s.directory, Owner: spawnOwner{PID: s.cmd.Process.Pid, Start: s.processIdentity}}, Env: append([]string(nil), s.environment...), Spawn: &spawnConfig{Local: true, Name: request.Name}}
	s.mu.Unlock()
	if request.Action == "spawn" {
		return runLocalSpawnRoute(c, req, func(mx *smux.Session) error {
			stream, err := mx.OpenStream()
			if err != nil {
				return err
			}
			proxySpawn(c, stream)
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
		target, err = createRouteShell(req, false)
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
	proxySpawn(c, target)
	return nil
}

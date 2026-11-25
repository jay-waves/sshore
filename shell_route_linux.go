//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	forwardio "github.com/jay-waves/sshore/internal/forward"
	"golang.org/x/term"
)

type shellRoute struct {
	open         taskEndpoint
	resource     resourceEndpoint
	drain        context.Context
	streams      sync.WaitGroup
	childUID     string
	host         string
	confirmation *shellRouteConfirmation // consumed once under the shell lock
}

type shellRouteConfirmation struct {
	token  string
	result chan error
}

// Open an authenticated task endpoint within the scope of the parent capability.
func (s *remoteShell) connectShellRoute(c net.Conn, request message) {
	if checkPeerVersion(request.SoftwareVersion, "client") != nil || request.Instance != s.record.UID {
		_ = writeMessage(c, message{Type: "error", Error: "parent shell instance changed"})
		return
	}
	s.mu.Lock()
	route := s.shellRoutes[request.Token]
	if route != nil {
		route.streams.Add(1)
	}
	s.mu.Unlock()
	if route == nil {
		_ = writeMessage(c, message{Type: "error", Error: "shell route is unavailable"})
		return
	}
	defer route.streams.Done()
	stop := context.AfterFunc(route.drain, func() { c.Close() })
	defer stop()
	stream, err := route.open()
	if err != nil {
		_ = writeMessage(c, message{Type: "error", Error: "shell route is unavailable"})
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

func (s *remoteShell) requestShellRoute(c net.Conn, request message) {
	req := request.LocalRequest
	if checkPeerVersion(request.SoftwareVersion, "launcher") != nil || req == nil || req.Shell == nil || !filepath.IsAbs(req.Record.Directory) ||
		req.Shell.Command != "sh" && req.Shell.Command != "attach" {
		return
	}
	s.mu.Lock()
	parent := s.active
	s.mu.Unlock()
	if parent == nil {
		_ = writeMessage(c, message{Type: "fatal", Error: "managed attachment required"})
		return
	}
	if req.Shell.Local {
		child, err := s.createRouteShell(*req)
		if err != nil {
			_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
			return
		}
		resource := func(ctx context.Context) (net.Conn, error) { return openHostedEndpoint(ctx, child.record.UID) }
		open := func() (net.Conn, error) { return resource(context.Background()) }
		ready := message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: child.record.UID}
		if err := s.serveShellRoute(c, parent, req.Shell, open, resource, ready, child.done); err != nil {
			_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	_ = withRemoteShell(ctx, c, *req, &s.shellTask, func(open taskEndpoint, resource resourceEndpoint, ready message, done <-chan struct{}) error {
		return s.serveShellRoute(c, parent, req.Shell, open, resource, ready, done)
	})
}

func (s *remoteShell) serveShellRoute(c net.Conn, parent *attachment, config *shellConfig, open taskEndpoint, resource resourceEndpoint, ready message, targetDone <-chan struct{}) error {
	confirmed := &shellRouteConfirmation{token: resourceToken(), result: make(chan error, 1)}
	var token string
	var route *shellRoute
	s.mu.Lock()
	if s.active != parent {
		s.mu.Unlock()
		return fmt.Errorf("parent attachment changed")
	}
	host := config.Host
	if config.Local {
		host = ""
	}
	for key, candidate := range s.shellRoutes {
		if candidate.childUID == ready.Instance && candidate.host == host {
			token, route = key, candidate
			break
		}
	}
	newRoute := route == nil
	if newRoute {
		drain, cancel := context.WithCancel(context.Background())
		token = resourceToken()
		route = &shellRoute{open: open, resource: resource, drain: drain, childUID: ready.Instance, host: host}
		if s.shellRoutes == nil {
			s.shellRoutes = make(map[string]*shellRoute)
		}
		s.shellRoutes[token] = route
		defer func() {
			s.mu.Lock()
			delete(s.shellRoutes, token)
			s.mu.Unlock()
			cancel()
			route.streams.Wait()
		}()
	} else if route.confirmation != nil {
		s.mu.Unlock()
		return fmt.Errorf("child shell handoff already in progress")
	} else {
		// Keep the established route and its streams while renewing this
		// handoff's confirmation.
		defer func() {
			s.mu.Lock()
			if route.confirmation == confirmed {
				route.confirmation = nil
			}
			s.mu.Unlock()
		}()
	}
	route.confirmation = confirmed
	s.mu.Unlock()
	if err := writeMessage(c, ready); err != nil {
		return err
	}
	action := "remote"
	if config.Local {
		action = "local"
		if config.Command == "attach" {
			action = "attach"
		}
	} else if config.Command == "attach" && ready.Reason == "" {
		action = "remote_attach"
	}
	name := config.Name
	if ready.Reason != "" {
		name = ""
	}
	if err := parent.controlMessage(message{Type: "shell_route", Action: action, Token: token, HandoffToken: confirmed.token, Instance: ready.Instance, Name: name, SourceHost: config.Host, NoLogin: config.NoLogin, NoProfile: config.NoProfile, Pinned: config.Pin}); err != nil {
		return err
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case err := <-confirmed.result:
		if err != nil {
			return err
		}
	case <-s.done:
		return fmt.Errorf("parent shell exited")
	case <-targetDone:
		return fmt.Errorf("child shell ended before attachment")
	case <-timer.C:
		return fmt.Errorf("shell attachment timed out")
	}
	if err := writeMessage(c, message{Type: "handoff_accepted"}); err != nil {
		return err
	}
	_ = c.Close()
	if !newRoute {
		return nil
	}
	select {
	case <-targetDone:
	case <-s.done:
	}
	return nil
}

func handoffRemoteShell(o options) (bool, error) {
	socket := os.Getenv("SSHORE_SOCKET")
	if socket == "" || (o.command != "sh" && o.command != "attach") || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return false, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return true, err
	}
	if o.sshDirectory != "" {
		cwd = o.sshDirectory
	}
	config := o.shellConfig()
	req := &localDaemonRequest{Record: localShellRecord{Directory: cwd}, Env: os.Environ(), Shell: &config}
	if !o.local && (o.command == "sh" || o.openIfUnavailable) {
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
	if err := writeMessage(c, message{Type: "shell_route_request", SoftwareVersion: version, LocalRequest: req}); err != nil {
		return true, err
	}
	if _, err := negotiateShellSetup(c, o.host, false, o.openIfUnavailable); err != nil {
		return true, err
	}
	reply, err := readMessage(c)
	if err != nil {
		return true, err
	}
	if reply.Type != "handoff_accepted" {
		return true, fmt.Errorf("shell handoff: %s", reply.Error)
	}
	return true, nil
}

func (s *remoteShell) confirmShellRoute(c net.Conn, request message) {
	if checkPeerVersion(request.SoftwareVersion, "client") != nil || request.Instance != s.record.UID {
		_ = writeMessage(c, message{Type: "error", Error: "parent shell instance changed"})
		return
	}
	s.mu.Lock()
	route := s.shellRoutes[request.Token]
	if route == nil {
		s.mu.Unlock()
		_ = writeMessage(c, message{Type: "error", Error: "shell route expired"})
		return
	}
	if route.confirmation == nil || route.confirmation.token != request.HandoffToken {
		s.mu.Unlock()
		_ = writeMessage(c, message{Type: "error", Error: "shell route confirmation expired"})
		return
	}
	var err error
	if request.Error != "" {
		err = errors.New(request.Error)
	}
	route.confirmation.result <- err
	route.confirmation = nil
	s.mu.Unlock()
	_ = writeMessage(c, message{Type: "route_confirmed"})
}

func (s *remoteShell) createRouteShell(req localDaemonRequest) (*remoteShell, error) {
	if req.Shell.Command == "attach" {
		target, ok := s.tasks.lookup(req.Shell.Instance).(*remoteShell)
		if !ok {
			return nil, fmt.Errorf("choose a registered shell on this host with sshore attach :ID")
		}
		return target, nil
	}
	return s.tasks.registerHostedShell(hostedShellRequest{Directory: req.Record.Directory, Name: req.Shell.Name, Env: req.Env, parent: &s.shellTask})
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
	if s.pid == 0 {
		s.mu.Unlock()
		return fmt.Errorf("managed shell required")
	}
	s.mu.Unlock()
	if request.Action != "attach" {
		return fmt.Errorf("invalid shell selection")
	}
	instance, err := resolveSession(request.Name, request.Instance)
	if err != nil {
		return err
	}
	if instance == "" {
		return fmt.Errorf("shell is unavailable")
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

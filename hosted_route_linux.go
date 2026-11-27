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

	"github.com/jay-waves/sshore/internal/shell"
	"golang.org/x/term"
)

// A parent grants access to a target until its route is retired.
type shellRoute struct {
	target       *shellTarget
	drain        context.Context
	streams      sync.WaitGroup
	confirmation *shellRouteConfirmation // consumed once under the shell lock
}

type shellRouteConfirmation struct {
	token  string
	result chan error
}

// Open an authenticated task endpoint within the scope of the parent capability.
func (s *tmuxTask) connectShellRoute(c net.Conn, request message) {
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
	stream, err := route.target.open()
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
	relayRoute(c, stream)
}

func (s *tmuxTask) requestShellRoute(c net.Conn, request message) {
	req := request.LocalRequest
	if checkPeerVersion(request.SoftwareVersion, "launcher") != nil || req == nil || req.Shell == nil || !filepath.IsAbs(req.Record.Directory) ||
		req.Shell.Command != "sh" && req.Shell.Command != "attach" {
		return
	}
	s.mu.Lock()
	parent := s.runtime.Active()
	s.mu.Unlock()
	if parent == nil {
		_ = writeMessage(c, message{Type: "fatal", Error: "managed attachment required"})
		return
	}
	if req.Shell.Local {
		if err := s.localShellRoute(c, parent, *req); err != nil {
			_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.runtime.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	_ = withRemoteShell(ctx, c, *req, s, func(target *shellTarget) error {
		return s.serveShellRoute(c, parent, req.Shell, target)
	})
}

func (s *tmuxTask) localShellRoute(launcher net.Conn, parent *attachment, req localDaemonRequest) error {
	if !filepath.IsAbs(req.Record.Directory) {
		return fmt.Errorf("child shell requires an absolute directory")
	}
	child, err := s.createRouteShell(req)
	if err != nil {
		return err
	}
	target := &shellTarget{uid: child.record.UID, done: child.runtime.Done()}
	target.resource = func(ctx context.Context) (net.Conn, error) { return openHostedEndpoint(ctx, target.uid) }
	target.open = func() (net.Conn, error) { return target.resource(context.Background()) }
	return s.serveShellRoute(launcher, parent, req.Shell, target)
}

func (s *tmuxTask) serveShellRoute(launcher net.Conn, parent *attachment, config *shellConfig, target *shellTarget) error {
	confirmed := &shellRouteConfirmation{token: resourceToken(), result: make(chan error, 1)}
	var token string
	var route *shellRoute
	s.mu.Lock()
	if s.runtime.Active() != parent {
		s.mu.Unlock()
		return fmt.Errorf("parent attachment changed")
	}
	for key, candidate := range s.shellRoutes {
		if candidate.target.uid == target.uid && candidate.target.host == target.host {
			token, route = key, candidate
			break
		}
	}
	newRoute := route == nil
	if newRoute {
		drain, cancel := context.WithCancel(context.Background())
		token = resourceToken()
		route = &shellRoute{target: target, drain: drain}
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
	if launcher != nil {
		if err := writeMessage(launcher, target.ready()); err != nil {
			return err
		}
	}
	action := "remote"
	if config.Local {
		action = "local"
		if config.Command == "attach" {
			action = "attach"
		}
	} else if config.Command == "attach" {
		action = "remote_attach"
	}
	if err := parent.controlMessage(message{Type: "shell_route", Host: &hostAnnouncement{Info: target.hostInfo}, Action: action, Token: token, HandoffToken: confirmed.token, Instance: target.uid, Name: config.Name, SourceHost: config.Host, NoLogin: config.NoLogin, NoProfile: config.NoProfile, Pinned: config.Pin, Argv: config.Argv}); err != nil {
		return err
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case err := <-confirmed.result:
		if err != nil {
			return err
		}
	case <-s.runtime.Done():
		return fmt.Errorf("parent shell exited")
	case <-target.done:
		return fmt.Errorf("child shell ended before attachment")
	case <-timer.C:
		return fmt.Errorf("shell attachment timed out")
	}
	if launcher != nil {
		if err := writeMessage(launcher, message{Type: "handoff_accepted"}); err != nil {
			return err
		}
		_ = launcher.Close()
	}
	if !newRoute {
		return nil
	}
	select {
	case <-target.done:
	case <-s.runtime.Done():
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
	if !o.local && (o.command == "sh") {
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
	if _, err := negotiateShellSetup(c, o.host, false); err != nil {
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

func (s *tmuxTask) confirmShellRoute(c net.Conn, request message) {
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

func (s *tmuxTask) createRouteShell(req localDaemonRequest) (*tmuxTask, error) {
	if req.Shell.Command == "attach" {
		target, ok := s.registry.lookup(req.Shell.Instance).(*tmuxTask)
		if !ok || !target.hosted {
			return nil, fmt.Errorf("choose a registered shell on this host with sshore attach ID")
		}
		return target, nil
	}
	directory, err := shell.ResolveStartupDirectory(req.Record.Directory, req.Shell.StartupDirectory)
	if err != nil {
		return nil, err
	}
	return s.registry.registerHostedShell(hostedShellRequest{Directory: directory, Name: req.Shell.Name, Env: req.Env, parent: s})
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
func (s *tmuxTask) connectRouteShell(c net.Conn, request message) (err error) {
	defer func() {
		if err != nil {
			_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
		}
	}()
	if err = checkPeerVersion(request.SoftwareVersion, "client"); err != nil {
		return err
	}
	s.mu.Lock()
	if !s.runtime.Started() {
		s.mu.Unlock()
		return fmt.Errorf("managed shell required")
	}
	s.mu.Unlock()
	if request.Action != "attach" {
		return fmt.Errorf("invalid shell selection")
	}
	selected, err := resolveHostedShell(request.Name, request.Instance)
	if err != nil {
		return err
	}
	if selected.Instance == "" {
		return fmt.Errorf("shell is unavailable")
	}
	target, err := openHostedEndpoint(context.Background(), selected.Instance)
	if err != nil {
		return err
	}
	defer target.Close()
	if err := writeMessage(c, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: selected.Instance}); err != nil {
		return err
	}
	relayRoute(c, target)
	return nil
}

type hostedRoutes struct {
	shellRoutes map[string]*shellRoute
}

func (s *tmuxTask) hostedAttachmentCommand(p *attachment, m message) error {
	switch m.Type {
	case "sh_child":
		s.mu.Lock()
		req := &localDaemonRequest{Record: localShellRecord{Directory: s.directoryOrStartupLocked()}, Env: append([]string(nil), s.environment...), Shell: &shellConfig{Command: "sh", Local: true, Pin: m.Pinned}}
		s.mu.Unlock()
		if m.Directory != "" {
			req.Record.Directory = m.Directory
		}
		go func() { p.childResult(s.localShellRoute(nil, p, *req)) }()
	default:
		return attachmentApprovalCommand(p, m)
	}
	return nil
}

func (s *tmuxTask) serveHostedIngress(c net.Conn, first message) error {
	if first.Type == "shell_owner_access" {
		owned := &ownedShellConnection{Conn: c, token: first.Token}
		s.mu.Lock()
		allowed := s.remoteOwner != "" && s.checkRemoteOwnerLocked(owned) == nil
		s.mu.Unlock()
		if !allowed || first.SoftwareVersion != version || first.Instance != s.record.UID {
			return writeMessage(c, message{Type: "error", Error: "shell owner route unavailable"})
		}
		if err := writeMessage(c, message{Type: "shell_owner_access_ready", SoftwareVersion: version}); err != nil {
			return err
		}
		var err error
		first, err = readMessage(c)
		if err != nil {
			return err
		}
		c = owned
	}
	if handled, err := s.serveHostedTask(c, first, s.relayResourceRoute); handled {
		return err
	}
	switch first.Type {
	case "shell_connect":
		return s.connectRouteShell(c, first)
	case "shell_route_confirm":
		s.confirmShellRoute(c, first)
	case "shell_route_request":
		s.requestShellRoute(c, first)
	case "shell_route_connect":
		s.connectShellRoute(c, first)
	default:
		return writeMessage(c, message{Type: "fatal", Error: "unsupported connection; update sshore on both computers"})
	}
	return nil
}

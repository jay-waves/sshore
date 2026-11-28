package main

import (
	"context"
	"fmt"
	"net"

	"github.com/jay-waves/sshore/internal/tmux"
)

func (s *shellTask) relayResourceRoute(c net.Conn, request message) error {
	hop := request.ResourceRoute[0]
	if len(request.ResourceRoute) >= maxConnectionHops || hop.Parent != s.record.UID {
		return fmt.Errorf("resource route parent changed")
	}
	request.ResourceRoute = request.ResourceRoute[1:]
	next := request.Instance
	if len(request.ResourceRoute) != 0 {
		next = request.ResourceRoute[0].Parent
	}
	var route *shellRoute
	if err := s.session.Inspect(func(state tmux.Snapshot) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		route = s.shellRoutes[hop.Token]
		if state.State == tmux.StateExited || state.Stopping || route == nil || route.target.record.UID != next || route.drain.Err() != nil {
			return fmt.Errorf("resource route is unavailable")
		}
		return nil
	}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(route.drain, requestTimeout)
	defer cancel()
	stream, err := openHostedEndpoint(ctx, next)
	if err != nil {
		return fmt.Errorf("open independent resource route: %w", err)
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	reply, err := exchangeRouteRequest(stream, request)
	if err != nil {
		stop()
		return err
	}
	if !stop() {
		return ctx.Err()
	}
	if err := checkPeerVersion(reply.SoftwareVersion, "resource route"); err != nil {
		return err
	}
	if err := writeMessage(c, reply); err != nil || reply.Type != "resource_ready" {
		return nil
	}
	cancel()
	relayRoute(c, stream)
	return nil
}

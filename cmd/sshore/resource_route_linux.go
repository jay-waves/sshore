package main

import (
	"context"
	"fmt"
	"net"
)

func (s *tmuxTask) relayResourceRoute(c net.Conn, request message) error {
	hop := request.ResourceRoute[0]
	if len(request.ResourceRoute) >= maxConnectionHops || hop.Parent != s.record.UID {
		return fmt.Errorf("resource route parent changed")
	}
	request.ResourceRoute = request.ResourceRoute[1:]
	next := request.Instance
	if len(request.ResourceRoute) != 0 {
		next = request.ResourceRoute[0].Parent
	}
	s.mu.Lock()
	route := s.shellRoutes[hop.Token]
	valid := !s.runtime.Exited() && !s.runtime.Stopping() && route != nil && route.target.uid == next && route.drain.Err() == nil
	s.mu.Unlock()
	if !valid {
		return fmt.Errorf("resource route is unavailable")
	}
	ctx, cancel := context.WithTimeout(route.drain, requestTimeout)
	defer cancel()
	stream, err := route.target.resource(ctx)
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

//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	forwardio "github.com/jay-waves/sshore/internal/forward"
)

type resourceApproval struct {
	id string
	op operation
}

func (s *remoteShell) serveRoutedResource(c net.Conn, request message) {
	fail := func(err error) {
		_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
	}
	if err := checkPeerVersion(request.SoftwareVersion, "resource client"); err != nil {
		fail(err)
		return
	}
	if !validTaskUID(request.Instance) || os.Getenv("SSHORE_DISABLED") == "1" {
		fail(fmt.Errorf("resource shell changed or unavailable"))
		return
	}
	if request.Type == "route_files" {
		if len(request.ResourceRoute) != 0 {
			fail(fmt.Errorf("invalid file route"))
			return
		}
		if err := s.checkApprovalSource(request); err != nil {
			fail(err)
			return
		}
		if writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}) == nil {
			serveApprovedFileEndpoint(c)
		}
		return
	}
	if len(request.Operations) != 1 || validateOperations(request.Operations) != nil {
		fail(fmt.Errorf("invalid resource operation"))
		return
	}
	op := request.Operations[0]
	switch request.Type {
	case "route_forward":
		if op.Kind != "forward" || op.LocalPortPending() {
			fail(fmt.Errorf("invalid forward request"))
			return
		}
	case "route_mount":
		if op.Kind != "mount" {
			fail(fmt.Errorf("invalid mount request"))
			return
		}
	default:
		fail(fmt.Errorf("invalid resource request"))
		return
	}
	if len(request.ResourceRoute) != 0 {
		if err := s.relayResourceRoute(c, request); err != nil {
			fail(err)
		}
		return
	}
	if err := s.consumeResourceApproval(request, op); err != nil {
		fail(err)
		return
	}
	if request.Type == "route_forward" {
		serveRoutedForward(c, op)
		return
	}
	if writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}) == nil {
		approval := files.MountApproval{
			Session: request.Name, PID: request.PID, ID: request.ApprovalID, Instance: request.Instance,
			Path: op.Path, ReadOnly: op.ReadOnly,
		}
		account, _ := localMachineIdentity()
		_ = files.ServeApprovedMount(c, version, account, beginIncomingMountTask, approval)
	}
}

func (s *remoteShell) relayResourceRoute(c net.Conn, request message) error {
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
	valid := !s.exited && !s.killing && route != nil && route.childUID == next && route.drain.Err() == nil
	s.mu.Unlock()
	if !valid {
		return fmt.Errorf("resource route is unavailable")
	}
	ctx, cancel := context.WithTimeout(route.drain, requestTimeout)
	defer cancel()
	stream, err := route.resource(ctx)
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
	if request.Type == "route_forward" && reply.Type == "resource_ready" {
		dialer, ok := stream.(forwardio.Dialer)
		if !ok {
			dialer = &net.Dialer{}
		} // Same-host child endpoint.
		cancel()
		return forwardio.Relay(dialer, reply.ForwardSocket, request.Operations[0].TargetPort, func(ep string) error {
			if err := writeForwardEndpoint(c, ep); err != nil {
				return err
			}
			forwardio.Bridge(c, stream)
			return nil
		})
	}
	if err := writeMessage(c, reply); err != nil || reply.Type != "resource_ready" {
		return nil
	}
	cancel()
	forwardio.Bridge(c, stream)
	return nil
}

func (s *remoteShell) consumeResourceApproval(request message, op operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || s.killing || s.active != nil || s.record.UID != request.Instance || s.record.Name != request.Name || s.pid == 0 || s.pid != request.PID {
		return fmt.Errorf("approved shell changed or remains attached; request the operation again")
	}
	grant := s.resourceApproval
	// PTY selection sets only the origin port. The remote port and direction
	// must match the proposal retained by the source shell.
	if op.Kind == "forward" {
		if op.Reverse {
			grant.op.TargetPort = op.TargetPort
		} else {
			grant.op.Port = op.Port
		}
	}
	if grant.id == "" || grant.id != request.ApprovalID || grant.op != op {
		return fmt.Errorf("resource approval changed or was already used; request the operation again")
	}
	s.resourceApproval = resourceApproval{}
	return nil
}

func serveApprovedFileEndpoint(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(requestTimeout))
	m, err := readMessage(c)
	if err != nil {
		return
	}
	if m.Type != "approved_transfer" {
		_ = writeMessage(c, message{Type: "error", Error: "invalid transfer request"})
		return
	}
	approvedRemoteTransfer(c, m)
}

func serveRoutedForward(c io.ReadWriteCloser, forward operation) {
	published := false
	err := forwardio.Serve(forward.Reverse, forward.Port, func(ep string) error {
		published = true
		if err := writeForwardEndpoint(c, ep); err != nil {
			return err
		}
		return watchPeerRecords(c, "forward", nil)
	})
	if err != nil && !published {
		_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
	}
}

func writeForwardEndpoint(c io.Writer, socket string) error {
	return writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version, ForwardSocket: socket})
}

package main

import (
	"context"
	"fmt"
	"io"
	"net"

	forwardio "github.com/jay-waves/sshore/internal/forward"
)

// A routed resource reuses the authenticated path. Host is a display label here.
func routedResourceOptions(connection recentConnection, kind string) (options, error) {
	if err := validateRecentConnection(connection); err != nil {
		return options{}, err
	}
	if connection.Instance == "" || !connection.viaShell() && kind != "route_files" {
		return options{}, fmt.Errorf("routed resource requires a live shell route")
	}
	o := options{command: "attach", host: connection.Host, session: connection.Session}
	applyConnectionOptions(&o, connection)
	return o, nil
}

func openRoutedResource(ctx context.Context, connection recentConnection, request message) (net.Conn, error) {
	o, err := routedResourceOptions(connection, request.Type)
	if err != nil {
		return nil, err
	}
	return openResourceEndpoint(ctx, &o, request, nil)
}

func openResourceEndpoint(ctx context.Context, o *options, request message, transports clientTransports) (_ net.Conn, err error) {
	request.Instance = o.instance
	c, err := openClientTransportContext(ctx, o, transports)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			c.Close()
		}
	}()
	reply, err := exchangeRouteRequest(c, request)
	if err != nil {
		return nil, err
	}
	if err := checkPeerVersion(reply.SoftwareVersion, "resource daemon"); err != nil {
		return nil, err
	}
	if reply.Type != "resource_ready" {
		return nil, fmt.Errorf("routed resource: %s", reply.Error)
	}
	return c, nil
}

func startRoutedForward(ctx context.Context, f forwardConfig) (*forwardConnection, error) {
	request := message{Type: "route_forward", Name: f.Session, PID: f.ResumePID, ApprovalID: f.ApprovalID, Operations: []operation{{Kind: "forward", Port: f.Port, TargetPort: f.Target, Reverse: f.Reverse}}}
	return openForwardStream(ctx, f, func() (io.ReadWriteCloser, error) {
		return openRoutedResource(ctx, *f.Connection, request)
	})
}

func openForwardStream(ctx context.Context, f forwardConfig, open func() (io.ReadWriteCloser, error)) (*forwardConnection, error) {
	var listener net.Listener
	// Bind the origin listener before consuming the remote approval.
	if !f.Reverse {
		var err error
		listener, err = net.Listen("tcp", loopback(f.Port))
		if err != nil {
			return nil, err
		}
		defer func() {
			if listener != nil {
				_ = listener.Close()
			}
		}()
	}
	conn, err := open()
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	forward, control, err := forwardio.Open(conn, listener, f.Target, true)
	if err != nil {
		return nil, err
	}
	listener = nil // The forwarding workers now own the listener.
	return &forwardConnection{
		Reader: control, Writer: control,
		Wait:  func() error { forward.Wait(); return nil },
		close: forward.Close,
	}, nil
}

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"

	"github.com/jay-waves/sshore/internal/wire"
	"github.com/xtaci/smux"
)

// A routed resource reuses the authenticated path. Host is a display label here.
func openRoutedResource(ctx context.Context, connection recentConnection, request message) (_ io.ReadWriteCloser, err error) {
	if err := validateRecentConnection(connection); err != nil {
		return nil, err
	}
	if connection.Instance == "" || !connection.viaShell() && request.Type != "route_files" {
		return nil, fmt.Errorf("routed resource requires a live shell route")
	}
	o := options{command: "attach", host: connection.Host, session: connection.Session}
	applyConnectionOptions(&o, connection)
	c, err := openClientTransportContext(ctx, &o, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			c.Close()
		}
	}()
	request.Instance = connection.Instance
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
	var listener net.Listener
	// Bind the origin listener before consuming the remote approval.
	if !f.Reverse {
		var err error
		listener, err = net.Listen("tcp", loopbackPort(f.Port))
		if err != nil {
			return nil, err
		}
		defer func() {
			if listener != nil {
				_ = listener.Close()
			}
		}()
	}
	conn, err := openRoutedResource(ctx, *f.Connection, request)
	if err != nil {
		return nil, err
	}
	forward, control, err := openResourceMuxForward(conn, listener, f.Target, true)
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

// Take ownership of the connection immediately and of the listener once the
// control stream is established. The control stream always precedes data.
func openResourceMuxForward(conn io.ReadWriteCloser, listener net.Listener, target int, initiator bool) (*wire.Forwarder, *smux.Stream, error) {
	openMux := smux.Server
	if initiator {
		openMux = smux.Client
	}
	mx, err := openMux(conn, wire.MuxConfig())
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	openControl := mx.AcceptStream
	if initiator {
		openControl = mx.OpenStream
	}
	control, err := openControl()
	if err != nil {
		_ = mx.Close()
		return nil, nil, err
	}
	return wire.StartMuxForward(mx, listener, target), control, nil
}

func loopbackPort(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

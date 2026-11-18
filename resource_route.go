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
func openRoutedResource(ctx context.Context, connection recentConnection, request message) (io.ReadWriteCloser, error) {
	if err := validateRecentConnection(connection); err != nil {
		return nil, err
	}
	if connection.Instance == "" || !connection.viaShell() && request.Type != "route_files" {
		return nil, fmt.Errorf("routed resource requires a live shell route")
	}
	o := options{command: "attach", host: connection.Host, session: connection.Session}
	applyConnectionOptions(&o, connection)
	c, err := openClientTransportContext(ctx, &o)
	if err != nil {
		return nil, err
	}
	request.Instance, request.SoftwareVersion = connection.Instance, version
	if err = writeMessage(c, request); err == nil {
		var reply message
		reply, err = readMessage(c)
		if err == nil {
			err = checkPeerVersion(reply.SoftwareVersion, "resource daemon")
		}
		if err == nil && reply.Type != "resource_ready" {
			err = fmt.Errorf("routed resource: %s", reply.Error)
		}
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func startRoutedForward(ctx context.Context, f forwardConfig) (result *forwardConnection, err error) {
	request := message{Type: "route_forward", Name: f.Session, PID: f.ResumePID, ApprovalID: f.ApprovalID, Operations: []operation{{Kind: "forward", Port: f.Port, TargetPort: f.Target, Reverse: f.Reverse}}}
	var listener net.Listener
	var conn io.ReadWriteCloser
	var mx *smux.Session
	// Startup owns all partial resources until the forwarder takes over.
	defer func() {
		if result != nil {
			return
		}
		if mx != nil {
			mx.Close()
		}
		if conn != nil {
			conn.Close()
		}
		if listener != nil {
			listener.Close()
		}
	}()
	// Bind the origin listener before consuming the remote approval.
	if !f.Reverse {
		listener, err = net.Listen("tcp", loopbackPort(f.Port))
		if err != nil {
			return nil, err
		}
	}
	conn, err = openRoutedResource(ctx, *f.Connection, request)
	if err != nil {
		return nil, err
	}
	mx, err = smux.Client(conn, wire.MuxConfig())
	if err != nil {
		return nil, err
	}
	control, err := mx.OpenStream()
	if err != nil {
		return nil, err
	}
	forward := wire.StartMuxForward(mx, listener, f.Target)
	return &forwardConnection{
		Input: control, Output: control,
		Wait:  func() error { forward.Wait(); return nil },
		Close: func() error { forward.Close(); return conn.Close() },
	}, nil
}

func loopbackPort(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

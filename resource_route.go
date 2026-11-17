package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
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
	wire, closeTransport, err := openClientTransportContext(ctx, &o)
	if err != nil {
		return nil, err
	}
	c := &routedResourceConn{ReadWriteCloser: wire, closeTransport: closeTransport}
	lifetime, cancel := context.WithCancel(ctx)
	c.stop = cancel
	context.AfterFunc(lifetime, func() { c.Close() })
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

type routedResourceConn struct {
	io.ReadWriteCloser
	closeTransport func()
	stop           func()
	once           sync.Once
}

func (c *routedResourceConn) Close() error {
	c.once.Do(func() {
		if c.stop != nil {
			c.stop()
		}
		c.closeTransport()
	})
	return nil
}

func startRoutedForward(ctx context.Context, f forwardConfig) (*forwardConnection, error) {
	request := message{Type: "route_forward", Name: f.Session, PID: f.ResumePID, ApprovalID: f.ApprovalID, Operations: []operation{{Kind: "forward", Port: f.Port, TargetPort: f.Target, Reverse: f.Reverse}}}
	// Bind the origin listener before consuming the remote approval.
	var listener net.Listener
	var err error
	if !f.Reverse {
		listener, err = net.Listen("tcp", loopbackPort(f.Port))
		if err != nil {
			return nil, err
		}
	}
	wire, err := openRoutedResource(ctx, *f.Connection, request)
	if err != nil {
		if listener != nil {
			listener.Close()
		}
		return nil, err
	}
	mx, err := smux.Client(wire, sshio.MuxConfig())
	if err != nil {
		wire.Close()
		if listener != nil {
			listener.Close()
		}
		return nil, err
	}
	control, err := mx.OpenStream()
	if err != nil {
		mx.Close()
		wire.Close()
		if listener != nil {
			listener.Close()
		}
		return nil, err
	}
	startForwardStreams(mx, listener, f.Target)
	return &forwardConnection{Input: control, Output: control, Wait: func() error { <-mx.CloseChan(); return nil }, Close: func() error { mx.Close(); return wire.Close() }}, nil
}

func loopbackPort(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// The listening endpoint opens streams; its peer dials only its own loopback.
// Every TCP connection ends with the owning mux, including blocked reads.
func startForwardStreams(mx *smux.Session, listener net.Listener, target int) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-mx.CloseChan()
		cancel()
		if listener != nil {
			listener.Close()
		}
	}()
	go func() {
		for {
			var conn net.Conn
			var stream *smux.Stream
			var err error
			if listener != nil {
				conn, err = listener.Accept()
				if err != nil {
					mx.Close()
					return
				}
				stream, err = mx.OpenStream()
				if err != nil {
					conn.Close()
					return
				}
			} else {
				stream, err = mx.AcceptStream()
				if err != nil {
					return
				}
			}
			go func(conn net.Conn, stream *smux.Stream) {
				defer stream.Close()
				if conn == nil {
					var err error
					conn, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", loopbackPort(target))
					if err != nil {
						return
					}
				}
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(stream, conn)
					_ = stream.CloseWrite()
					close(done)
				}()
				_, _ = io.Copy(conn, stream)
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				} else {
					_ = conn.Close()
				}
				<-done
			}(conn, stream)
		}
	}()
}

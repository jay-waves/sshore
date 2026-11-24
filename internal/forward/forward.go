package forward

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An SSH client opens a fresh native channel for each DialContext call.
// A local daemon hop uses a net.Dialer through the same interface.
type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

func Loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

type dialFunc func(context.Context) (net.Conn, error)

// The final forward hop dials the approved TCP port directly. A relay or
// reverse listener publishes a socket inside its task's private directory.
func destination(client Dialer, socket string, target int) (dialFunc, error) {
	if socket == "" {
		return func(ctx context.Context) (net.Conn, error) {
			return client.DialContext(ctx, "tcp", Loopback(target))
		}, nil
	}
	dir := path.Dir(socket)
	if path.Clean(socket) != socket || path.Base(socket) != "data" ||
		path.Dir(dir) != "/tmp" || !strings.HasPrefix(path.Base(dir), "sshore-fwd-") {
		return nil, fmt.Errorf("invalid forward socket")
	}
	return func(ctx context.Context) (net.Conn, error) {
		return client.DialContext(ctx, "unix", socket)
	}, nil
}

func relayConnections(parent context.Context, accept func() (net.Conn, error), dial dialFunc, signalAccept bool) {
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer cancel()
	for {
		conn, err := accept()
		if err != nil {
			return
		}
		workers.Go(func() {
			defer conn.Close()
			peer, err := dial(ctx)
			if err != nil {
				return
			}
			defer peer.Close()
			if signalAccept {
				if _, err := conn.Write([]byte{1}); err != nil {
					return
				}
			}
			Splice(ctx, conn, peer)
		})
	}
}

// serveSocket exposes one task-private endpoint to the previous SSH hop.
func serveSocket(dial dialFunc, signalAccept bool, control func(string) error) error {
	dir, err := os.MkdirTemp("/tmp", "sshore-fwd-")
	if err != nil {
		return err
	}
	defer os.Remove(dir)
	socket := path.Join(dir, "data")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); relayConnections(ctx, listener.Accept, dial, signalAccept) }()
	err = control(socket)
	cancel()
	listener.Close()
	<-done
	return err
}

// Relay joins channels on adjacent, independently authenticated SSH hops.
func Relay(client Dialer, socket string, target int, control func(string) error) error {
	dial, err := destination(client, socket, target)
	if err != nil {
		return err
	}
	return serveSocket(dial, false, control)
}

// Serve binds the public reverse listener itself so sshd's GatewayPorts
// setting cannot broaden its scope.
func Serve(reverse bool, port int, control func(string) error) error {
	if !reverse {
		return control("")
	}
	listener, err := net.Listen("tcp4", Loopback(port))
	if err != nil {
		return err
	}
	defer listener.Close()
	return serveSocket(func(context.Context) (net.Conn, error) { return listener.Accept() }, true, func(socket string) error {
		err := control(socket)
		listener.Close()
		return err
	})
}

// Start owns a single forwarding rule. Shutdown closes the task transport
// before joining workers so that pending SSH channel opens also unblock.
func Start(ctx context.Context, client Dialer, port, target int, reverse bool, open func() (io.ReadWriteCloser, string, error)) (_ *Session, err error) {
	closeTransport := sync.OnceFunc(func() {
		if c, ok := client.(io.Closer); ok {
			_ = c.Close()
		}
	})
	stopSetup := context.AfterFunc(ctx, closeTransport)
	defer stopSetup()
	var listener net.Listener
	if !reverse {
		listener, err = net.Listen("tcp4", Loopback(port))
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				listener.Close()
			}
		}()
	}
	control, socket, err := open()
	if err != nil {
		return nil, err
	}
	dial, err := destination(client, socket, target)
	if err == nil && reverse && socket == "" {
		err = fmt.Errorf("reverse forward listener missing")
	}
	if err != nil {
		control.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	closeAll := sync.OnceFunc(func() {
		cancel()
		if listener != nil {
			listener.Close()
		}
		closeTransport()
		control.Close()
	})
	s := &Session{Writer: control, stop: closeAll, done: make(chan struct{})}
	stop := context.AfterFunc(ctx, closeAll)
	go func() {
		defer close(s.done)
		defer stop()
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			_, _ = io.Copy(io.Discard, control)
			closeAll()
		}()
		if listener != nil {
			relayConnections(ctx, listener.Accept, dial, false)
		} else {
			local := net.Dialer{Timeout: 5 * time.Second}
			relayConnections(ctx, func() (net.Conn, error) { return acceptReverse(ctx, dial) }, func(ctx context.Context) (net.Conn, error) {
				return local.DialContext(ctx, "tcp", Loopback(target))
			}, false)
		}
		closeAll()
		<-readDone
	}()
	return s, nil
}

// A reverse channel becomes an accepted client only after the remote
// loopback listener has a connection. Idle forwards never dial the service.
func acceptReverse(ctx context.Context, dial dialFunc) (net.Conn, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	var ready [1]byte
	if _, err = io.ReadFull(conn, ready[:]); err == nil && ready[0] == 1 {
		return conn, nil
	}
	conn.Close()
	if err == nil {
		err = fmt.Errorf("invalid reverse forward response")
	}
	return nil, err
}

type Session struct {
	io.Writer
	stop func()
	done chan struct{}
}

func (s *Session) Wait() error  { <-s.done; return nil }
func (s *Session) Close() error { s.stop(); return s.Wait() }

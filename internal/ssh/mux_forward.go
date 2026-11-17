package ssh

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

// MuxForward owns a resource mux, its optional loopback listener and all data
// connections. The listening side opens streams; the other side dials its own
// loopback target. Control streams must be established before startup.
type MuxForward struct {
	mux  *smux.Session
	done chan struct{}
}

// StartMuxForward transfers listener and mux ownership to the forwarder. A nil
// listener selects the dialing side. Target must already be validated by the
// application; this component does not perform resource approval or handshakes.
func StartMuxForward(mx *smux.Session, listener net.Listener, target int) *MuxForward {
	f := &MuxForward{mux: mx, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if listener != nil {
			// CloseChan alone does not report a socket read error until smux's
			// keepalive expires. This side never accepts peer-initiated data
			// streams, so AcceptStream detects loss even with an idle listener.
			// Any unsolicited stream also terminates this dedicated resource mux.
			_, _ = mx.AcceptStream()
			_ = mx.Close()
		} else {
			<-mx.CloseChan()
		}
		cancel()
		if listener != nil {
			_ = listener.Close()
		}
	}()
	go func() {
		var workers sync.WaitGroup
		defer close(f.done)
		defer func() { <-stopped; workers.Wait() }()
		defer mx.Close()
		for {
			var conn net.Conn
			var stream *smux.Stream
			var err error
			if listener != nil {
				conn, err = listener.Accept()
				if err != nil {
					return
				}
				stream, err = mx.OpenStream()
				if err != nil {
					_ = conn.Close()
					return
				}
			} else {
				stream, err = mx.AcceptStream()
				if err != nil {
					return
				}
			}
			workers.Go(func() {
				defer stream.Close()
				if conn == nil {
					var err error
					address := net.JoinHostPort("127.0.0.1", strconv.Itoa(target))
					conn, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
					if err != nil {
						return
					}
				}
				Splice(ctx, conn, stream)
			})
		}
	}()
	return f
}

// Wait returns only after the listener and every data connection have stopped.
func (f *MuxForward) Wait() { <-f.done }

// Close aborts the mux and joins its forwarding workers. It is safe to repeat.
func (f *MuxForward) Close() error {
	_ = f.mux.Close()
	f.Wait()
	return nil
}

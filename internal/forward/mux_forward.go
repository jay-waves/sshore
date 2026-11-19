package forward

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

// Forwarder owns the inputs and active connections of a forwarding rule.
type Forwarder struct {
	stop func()
	done chan struct{}
}

func startForwarder(closeInputs func(), serve func(context.Context, *sync.WaitGroup, func())) *Forwarder {
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forwarder{
		stop: sync.OnceFunc(func() {
			cancel()
			closeInputs()
		}),
		done: make(chan struct{}),
	}
	go func() {
		var workers sync.WaitGroup
		serve(ctx, &workers, f.stop)
		f.Stop()
		workers.Wait()
		close(f.done)
	}()
	return f
}

// StartForwardListener takes ownership of a listener and relays each accepted
// connection to the supplied dial target.
func StartForwardListener(listener net.Listener, dial func(context.Context) (net.Conn, error)) *Forwarder {
	return startForwarder(func() { _ = listener.Close() }, func(ctx context.Context, workers *sync.WaitGroup, _ func()) {
		forwardAccepted(ctx, workers, listener.Accept, dial)
	})
}

// startMuxForward takes ownership of a mux with its control streams already
// established. It opens a stream for each listener connection, or dials the
// validated loopback target for each peer stream if listener is nil.
func startMuxForward(mx *smux.Session, listener net.Listener, target int) *Forwarder {
	if listener != nil {
		return startForwarder(func() {
			_ = listener.Close()
			_ = mx.Close()
		}, func(ctx context.Context, workers *sync.WaitGroup, stop func()) {
			// The TCP accept loop cannot see mux errors. AcceptStream detects
			// transport loss or an unsolicited peer stream even while idle.
			workers.Go(func() {
				_, _ = mx.AcceptStream()
				stop()
			})
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				stream, err := mx.OpenStream()
				if err != nil {
					_ = conn.Close()
					return
				}
				workers.Go(func() { Splice(ctx, conn, stream) })
			}
		})
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(target))
	dialer := net.Dialer{Timeout: 5 * time.Second}
	return startForwarder(func() { _ = mx.Close() }, func(ctx context.Context, workers *sync.WaitGroup, _ func()) {
		forwardAccepted(ctx, workers, mx.AcceptStream, func(ctx context.Context) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", address)
		})
	})
}

func forwardAccepted[Incoming io.ReadWriteCloser](ctx context.Context, workers *sync.WaitGroup, accept func() (Incoming, error), dial func(context.Context) (net.Conn, error)) {
	for {
		incoming, err := accept()
		if err != nil {
			return
		}
		workers.Go(func() {
			outgoing, err := dial(ctx)
			if err != nil {
				_ = incoming.Close()
				return
			}
			Splice(ctx, incoming, outgoing)
		})
	}
}

// Stop cancels forwarding and closes the inputs.
func (f *Forwarder) Stop() { f.stop() }

// Wait returns once all forwarding workers have exited.
func (f *Forwarder) Wait() { <-f.done }

// Close stops forwarding and joins its workers. It is safe to repeat.
func (f *Forwarder) Close() error {
	f.Stop()
	f.Wait()
	return nil
}

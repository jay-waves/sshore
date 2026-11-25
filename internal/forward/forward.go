package forward

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

func Loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// Start binds the listener before negotiating the rule. A zero listenPort
// makes this endpoint dial targetPort instead. The initiator opens the first
// stream for task metadata. The listener opens all subsequent streams.
func Start(parent context.Context, listenPort, targetPort int, initiator bool, open func() (io.ReadWriteCloser, error), watch func(io.Reader) error) (_ *Session, err error) {
	var listener net.Listener
	if listenPort != 0 {
		listener, err = net.Listen("tcp4", Loopback(listenPort))
		if err != nil {
			return nil, fmt.Errorf("listen on %s: %w", Loopback(listenPort), err)
		}
		defer func() {
			if err != nil {
				listener.Close()
			}
		}()
	}
	conn, err := open()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	stopSetup := context.AfterFunc(parent, func() { conn.Close() })
	defer stopSetup()

	config := smux.DefaultConfig()
	config.Version = 2 // A stalled connection must not exhaust the shared receive buffer.
	newMux := smux.Server
	if initiator {
		newMux = smux.Client
	}
	mux, err := newMux(conn, config)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			mux.Close()
		}
	}()

	getControl := mux.AcceptStream
	if initiator {
		getControl = mux.OpenStream
	}
	control, err := getControl()
	if err != nil {
		return nil, err
	}

	accept := mux.Accept
	dialer := net.Dialer{Timeout: 5 * time.Second}
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) {
		return dialer.DialContext(ctx, "tcp4", Loopback(targetPort))
	}
	if listener != nil {
		accept = func() (io.ReadWriteCloser, error) { return listener.Accept() }
		dial = func(context.Context) (io.ReadWriteCloser, error) { return mux.Open() }
	}

	ctx, cancel := context.WithCancel(parent)
	stop := sync.OnceFunc(func() {
		cancel()
		if listener != nil {
			listener.Close()
		}
		mux.Close()
	})
	stopOnCancel := context.AfterFunc(ctx, stop)
	s := &Session{Writer: control, stop: stop, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer stopOnCancel()
		controlDone := make(chan struct{})
		go func() {
			defer close(controlDone)
			if watch == nil {
				_, _ = io.Copy(io.Discard, control)
			} else {
				_ = watch(control)
			}
			stop()
		}()
		var workers sync.WaitGroup
		for {
			incoming, err := accept()
			if err != nil {
				break
			}
			workers.Go(func() {
				outgoing, err := dial(ctx)
				if err != nil {
					incoming.Close()
					return
				}
				Splice(ctx, incoming, outgoing)
			})
		}
		stop()
		workers.Wait()
		<-controlDone
	}()
	return s, nil
}

type Session struct {
	io.Writer
	stop func()
	done chan struct{}
}

func (s *Session) Wait() error  { <-s.done; return nil }
func (s *Session) Close() error { s.stop(); return s.Wait() }

package ports

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

// Start binds the listener before negotiating the rule. The initiator opens the first
// stream for task metadata. The listener opens all subsequent streams.
func (rule Rule) Start(parent context.Context, initiator bool, open func() (io.ReadWriteCloser, error), watch func(io.Reader) error) (_ *Session, err error) {
	listenPort, targetPort := rule.ports(!initiator)
	var tcp net.Listener
	var udp *net.UDPConn
	var listener io.Closer
	if listenPort != 0 {
		if rule.UDP {
			udp, err = net.ListenUDP("udp4", udpLoopback(listenPort))
			listener = udp
		} else {
			tcp, err = net.Listen("tcp4", Loopback(listenPort))
			listener = tcp
		}
		if err != nil {
			side := "remote"
			if initiator {
				side = "local"
			}
			return nil, fmt.Errorf("%s listener: %w", side, err)
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
		switch {
		case udp != nil:
			serveUDPListener(udp, mux, &workers)
		case tcp != nil:
			serveTCPListener(ctx, tcp, mux, &workers)
		default:
			rule.serveTarget(ctx, targetPort, mux, &workers)
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

func (s *Session) Wait()  { <-s.done }
func (s *Session) Close() { s.stop(); s.Wait() }

func (rule Rule) serveTarget(ctx context.Context, targetPort int, mux *smux.Session, workers *sync.WaitGroup) {
	for {
		stream, err := mux.AcceptStream()
		if err != nil {
			break
		}
		workers.Go(func() {
			if rule.UDP {
				relayUDPTarget(stream, targetPort)
				return
			}
			dialer := net.Dialer{Timeout: 5 * time.Second}
			conn, err := dialer.DialContext(ctx, "tcp4", Loopback(targetPort))
			if err != nil {
				stream.Close()
				return
			}
			relayTCP(ctx, conn, stream)
		})
	}
}

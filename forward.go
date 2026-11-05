package main

// Forwarding belongs to one attachment. Control requests use short-lived
// streams; each accepted TCP connection gets its own binary data stream.
import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

const maxForwardPorts = 8
const maxForwardConnections = 32
const forwardTimeout = 10 * time.Second
const forwardBlock = 32 * 1024

func parseForward(args []string) (message, error) {
	m := message{Type: "forward", Action: "add"}
	if len(args) == 1 && args[0] == "--list" {
		m.Action = "list"
		return m, nil
	}
	if len(args) == 2 && args[0] == "--stop" {
		m.Action = "stop"
		args = args[1:]
	}
	if len(args) != 1 {
		return m, usage("usage: sshore forward PORT | --stop PORT | --list")
	}
	for _, c := range args[0] {
		if c < '0' || c > '9' {
			return m, usage("port must be an integer from 1 to 65535")
		}
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 || n > 65535 {
		return m, usage("port must be an integer from 1 to 65535")
	}
	m.Port = n
	return m, nil
}
func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

type forwardRule struct {
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
}
type forwardManager struct {
	mu     sync.Mutex
	mux    *smux.Session
	ctx    context.Context
	cancel context.CancelFunc
	rules  map[int]*forwardRule
	slots  chan struct{}
}

func newForwardManager(parent context.Context, mux *smux.Session) *forwardManager {
	ctx, cancel := context.WithCancel(parent)
	return &forwardManager{mux: mux, ctx: ctx, cancel: cancel, rules: make(map[int]*forwardRule), slots: make(chan struct{}, maxForwardConnections)}
}
func (f *forwardManager) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancel()
	for port, r := range f.rules {
		r.cancel()
		_ = r.listener.Close()
		delete(f.rules, port)
	}
}
func (f *forwardManager) request(m message) message {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := message{Type: "forward_result", Port: m.Port}
	fail := func(err error) message { result.Error = err.Error(); return result }
	if f.ctx.Err() != nil {
		return fail(fmt.Errorf("connection closed"))
	}
	if m.Action == "list" {
		for p := range f.rules {
			result.Ports = append(result.Ports, p)
		}
		sort.Ints(result.Ports)
		return result
	}
	if m.Port < 1 || m.Port > 65535 {
		return fail(fmt.Errorf("invalid port"))
	}
	switch m.Action {
	case "add":
		if f.rules[m.Port] != nil {
			result.Name = "Already forwarding"
			return result
		}
		if len(f.rules) >= maxForwardPorts {
			return fail(fmt.Errorf("forwarding limit reached (%d ports)", maxForwardPorts))
		}
		ln, err := net.Listen("tcp4", loopback(m.Port))
		if err != nil {
			return fail(err)
		}
		ctx, cancel := context.WithCancel(f.ctx)
		r := &forwardRule{listener: ln, ctx: ctx, cancel: cancel}
		f.rules[m.Port] = r
		go f.accept(r, m.Port)
		result.Name = "Forwarding"
	case "stop":
		if r := f.rules[m.Port]; r != nil {
			r.cancel()
			_ = r.listener.Close()
			delete(f.rules, m.Port)
		}
		result.Name = "Stopped forwarding"
	default:
		return fail(fmt.Errorf("invalid forward action"))
	}
	return result
}
func (f *forwardManager) accept(r *forwardRule, port int) {
	for {
		c, err := r.listener.Accept()
		if err != nil {
			return
		}
		select {
		case f.slots <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		go func() {
			defer func() { <-f.slots; _ = c.Close() }()
			stop := context.AfterFunc(r.ctx, func() { _ = c.Close() })
			defer stop()
			st, err := f.mux.OpenStream()
			if err != nil {
				return
			}
			defer st.Close()
			abort := context.AfterFunc(r.ctx, func() { _ = st.SetDeadline(time.Now()); _ = st.Close() })
			defer abort()
			_ = st.SetDeadline(time.Now().Add(forwardTimeout))
			if err = writeMessage(st, message{Type: "forward_connect", Port: port}); err != nil {
				return
			}
			reply, err := readMessage(st)
			if err != nil || reply.Type != "forward_ready" || reply.Error != "" {
				return
			}
			_ = st.SetDeadline(time.Time{})
			relayForward(r.ctx, c, st)
		}()
	}
}
func (f *forwardManager) handle(st *smux.Stream, m message) {
	defer st.Close()
	_ = st.SetWriteDeadline(time.Now().Add(forwardTimeout))
	_ = writeMessage(st, f.request(m))
}

// A zero-length record is TCP write EOF. Stream EOF without that record is
// cancellation, so stopping a forward also closes a silent remote TCP peer.
// Payloads remain binary; record sizes are bounded before allocating/reading.
func relayForward(ctx context.Context, c net.Conn, st *smux.Stream) {
	abort := func() { _ = c.Close(); _ = st.SetDeadline(time.Now()); _ = st.Close() }
	stop := context.AfterFunc(ctx, abort)
	defer stop()
	done := make(chan error, 2)
	go func() {
		buf := make([]byte, forwardBlock)
		var header [4]byte
		for {
			n, err := c.Read(buf)
			if n > 0 {
				binary.BigEndian.PutUint32(header[:], uint32(n))
				if e := writeFull(st, header[:]); e != nil {
					done <- e
					return
				}
				if e := writeFull(st, buf[:n]); e != nil {
					done <- e
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					header = [4]byte{}
					err = writeFull(st, header[:])
				}
				done <- err
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, forwardBlock)
		var header [4]byte
		for {
			if _, err := io.ReadFull(st, header[:]); err != nil {
				done <- err
				return
			}
			n := binary.BigEndian.Uint32(header[:])
			if n == 0 {
				err := c.(*net.TCPConn).CloseWrite()
				done <- err
				if err == nil {
					// Keep observing cancellation after TCP write EOF. Otherwise
					// a half-closed, silent service could survive --stop forever.
					var extra [1]byte
					_, _ = st.Read(extra[:])
					_ = c.Close()
				}
				return
			}
			if n > forwardBlock {
				done <- fmt.Errorf("oversized forward record")
				return
			}
			if _, err := io.ReadFull(st, buf[:n]); err != nil {
				done <- err
				return
			}
			if err := writeFull(c, buf[:n]); err != nil {
				done <- err
				return
			}
		}
	}()
	if err := <-done; err != nil {
		abort()
	}
	<-done
}

func serveForwardStreams(ctx context.Context, mux *smux.Session) {
	slots := make(chan struct{}, maxForwardConnections)
	for {
		st, err := mux.AcceptStream()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = st.Close()
			continue
		}
		go func() {
			defer func() { <-slots; _ = st.Close() }()
			abort := context.AfterFunc(ctx, func() { _ = st.SetDeadline(time.Now()); _ = st.Close() })
			defer abort()
			_ = st.SetDeadline(time.Now().Add(forwardTimeout))
			m, err := readMessage(st)
			if err != nil || m.Type != "forward_connect" || m.Port < 1 || m.Port > 65535 {
				return
			}
			dialer := net.Dialer{Timeout: 5 * time.Second}
			c, err := dialer.DialContext(ctx, "tcp4", loopback(m.Port))
			reply := message{Type: "forward_ready"}
			if err != nil {
				reply.Error = err.Error()
				_ = writeMessage(st, reply)
				return
			}
			defer c.Close()
			if err = writeMessage(st, reply); err != nil {
				return
			}
			_ = st.SetDeadline(time.Time{})
			relayForward(ctx, c, st)
		}()
	}
}

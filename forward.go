package main

// Dynamic forwarding belongs to one attachment. Control requests use short-lived
// smux streams; each accepted TCP connection gets its own binary stream. Requests
// select an IPv4 loopback listener on either machine and a loopback target.
// Current-shell from/to operands are resolved before approval. Reverse streams
// can only dial the exact locally approved destination. An occupied listening
// port fails without substitution; opening a listener does not prove the remote
// service is running. A remote dial failure closes that connection, not the listener.
// Each accepting side allows at most 32 TCP connections; excess connections close. Remote
// stream handlers share that bound with approved transfers. Detach, shell
// exit, disconnection and --stop close listeners and existing connections. A new
// remote approval request detaches and therefore closes the previous rule.
// Native OpenSSH -L/-R/-D rules are separate and cannot be listed or stopped here;
// they share the same local port namespace and obey OpenSSH forwarding policy.

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

type forwardSpec struct {
	Reverse    bool   `json:"reverse,omitempty"`
	Port       int    `json:"port"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
}

func normalizeForward(port int, host string, remotePort int) forwardSpec {
	if host == "" {
		host = "127.0.0.1"
	}
	if remotePort == 0 {
		remotePort = port
	}
	return forwardSpec{Port: port, RemoteHost: host, RemotePort: remotePort}
}

func parsePort(s string) (int, error) {
	if s == "" {
		return 0, usage("port must be an integer from 1 to 65535")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, usage("port must be an integer from 1 to 65535")
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, usage("port must be an integer from 1 to 65535")
	}
	return n, nil
}

const maxForwardPorts = 8
const maxForwardConnections = 32
const forwardTimeout = 10 * time.Second
const forwardBlock = 32 * 1024

// parseForward interprets operands in the requesting remote shell's frame.
// Wire fields are then fixed: Reverse means server listener -> local client.
// Port is always the listener; RemotePort is always the approved destination.
func parseForward(args []string) (message, error) {
	m := message{Type: "forward", Action: "add", RemoteHost: "127.0.0.1"}
	if len(args) == 1 && args[0] == "--list" {
		m.Action = "list"
		return m, nil
	}
	if len(args) > 0 && args[0] == "--stop" {
		m.Action = "stop"
		args = args[1:]
	}
	if len(args) != 3 || (args[1] != "from" && args[1] != "to") {
		return m, usage("use forward CURRENT_PORT from|to PEER_PORT (both ports required); --stop uses the same operands")
	}
	current, e := parsePort(args[0])
	if e != nil {
		return m, e
	}
	peer, e := parsePort(args[2])
	if e != nil {
		return m, e
	}
	if args[1] == "to" {
		m.Port = current
		m.RemotePort = peer
		m.Reverse = true
	} else {
		m.Port = peer
		m.RemotePort = current
	}
	return m, nil
}

func describeForward(port int, host string, target int, reverse bool) string {
	spec := normalizeForward(port, host, target)
	if reverse {
		return fmt.Sprintf("server %s -> your computer %s", loopback(spec.Port), net.JoinHostPort(spec.RemoteHost, strconv.Itoa(spec.RemotePort)))
	}
	return fmt.Sprintf("your computer %s -> server %s", loopback(spec.Port), net.JoinHostPort(spec.RemoteHost, strconv.Itoa(spec.RemotePort)))
}

func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// A port can be used independently on each endpoint.
type forwardKey struct {
	port    int
	reverse bool
}

type forwardRule struct {
	reverse    bool
	listener   net.Listener
	ctx        context.Context
	cancel     context.CancelFunc
	remoteHost string
	remotePort int
}
type forwardManager struct {
	// Server-side managers mark outgoing connections; client-side managers require a grant.
	senderReverse bool
	mu            sync.Mutex
	mux           *smux.Session
	ctx           context.Context
	cancel        context.CancelFunc
	rules         map[forwardKey]*forwardRule
	slots         chan struct{}
}

func newForwardManager(parent context.Context, mux *smux.Session) *forwardManager {
	ctx, cancel := context.WithCancel(parent)
	return &forwardManager{mux: mux, ctx: ctx, cancel: cancel, rules: make(map[forwardKey]*forwardRule), slots: make(chan struct{}, maxForwardConnections)}
}
func (f *forwardManager) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancel()
	for port, r := range f.rules {
		r.cancel()
		if r.listener != nil {
			_ = r.listener.Close()
		}
		delete(f.rules, port)
	}
}
func (f *forwardManager) request(m message) message {
	f.mu.Lock()
	defer f.mu.Unlock()
	spec := normalizeForward(m.Port, m.RemoteHost, m.RemotePort)
	key := forwardKey{m.Port, m.Reverse}
	result := message{Reverse: m.Reverse, Type: "forward_result", Port: spec.Port, RemoteHost: spec.RemoteHost, RemotePort: spec.RemotePort}
	fail := func(err error) message { result.Error = err.Error(); return result }
	if f.ctx.Err() != nil {
		return fail(fmt.Errorf("connection closed"))
	}
	if m.Action == "list" {
		for key := range f.rules {
			result.Ports = append(result.Ports, key.port)
		}
		for key, r := range f.rules {
			spec := normalizeForward(key.port, r.remoteHost, r.remotePort)
			spec.Reverse = r.reverse
			result.Forwards = append(result.Forwards, spec)
		}
		sort.Ints(result.Ports)
		sort.Slice(result.Forwards, func(i, j int) bool {
			a, b := result.Forwards[i], result.Forwards[j]
			if a.Port == b.Port {
				return !a.Reverse && b.Reverse
			}
			return a.Port < b.Port
		})
		return result
	}
	if m.Port < 1 || m.Port > 65535 {
		return fail(fmt.Errorf("invalid port"))
	}
	switch m.Action {
	case "add":
		if old := f.rules[key]; old != nil {
			if old.remoteHost != spec.RemoteHost || old.remotePort != spec.RemotePort {
				return fail(fmt.Errorf("listener already forwards to a different destination"))
			}
			result.Name = "Already forwarding"
			return result
		}
		if len(f.rules) >= maxForwardPorts {
			return fail(fmt.Errorf("forwarding limit reached (%d ports)", maxForwardPorts))
		}
		if spec.RemotePort < 1 || spec.RemotePort > 65535 || spec.RemoteHost != "127.0.0.1" {
			return fail(fmt.Errorf("dynamic forwards require a valid loopback destination"))
		}
		if m.Reverse {
			ctx, cancel := context.WithCancel(f.ctx)
			f.rules[key] = &forwardRule{ctx: ctx, cancel: cancel, remoteHost: spec.RemoteHost, remotePort: spec.RemotePort, reverse: true}
			reply := f.remoteRequest(m)
			if reply.Error != "" {
				cancel()
				delete(f.rules, key)
			}
			return reply
		}
		ln, err := net.Listen("tcp4", loopback(m.Port))
		if err != nil {
			return fail(err)
		}
		ctx, cancel := context.WithCancel(f.ctx)
		r := &forwardRule{listener: ln, ctx: ctx, cancel: cancel, remoteHost: spec.RemoteHost, remotePort: spec.RemotePort}
		f.rules[key] = r
		go f.accept(r, m.Port)
		result.Name = "Forwarding"
	case "stop":
		if r := f.rules[key]; r != nil {
			if r.remotePort != spec.RemotePort {
				return fail(fmt.Errorf("forward destination does not match; use --list"))
			}
			if m.Reverse {
				reply := f.remoteRequest(m)
				if reply.Error != "" {
					return reply
				}
			}
			r.cancel()
			if r.listener != nil {
				_ = r.listener.Close()
			}
			delete(f.rules, key)
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
			if err = writeMessage(st, message{Type: "forward_connect", Reverse: f.senderReverse, Port: port, RemoteHost: r.remoteHost, RemotePort: r.remotePort}); err != nil {
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
	reverse := newForwardManager(ctx, mux)
	reverse.senderReverse = true
	defer reverse.close()
	slots := make(chan struct{}, maxForwardConnections)
	var workers sync.WaitGroup
	defer workers.Wait()
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
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-slots; _ = st.Close() }()
			abort := context.AfterFunc(ctx, func() { _ = st.SetDeadline(time.Now()); _ = st.Close() })
			defer abort()
			_ = st.SetDeadline(time.Now().Add(forwardTimeout))
			m, err := readMessage(st)
			if err == nil && m.Type == "approved_transfer" {
				approvedRemoteTransfer(st, m)
				return
			}
			if err == nil && m.Type == "forward_control" && !m.Reverse && (m.Action == "add" || m.Action == "stop") {
				_ = writeMessage(st, reverse.request(m))
				return
			}
			if err != nil || m.Type != "forward_connect" || m.Reverse {
				return
			}
			spec := normalizeForward(m.Port, m.RemoteHost, m.RemotePort)
			if spec.Port < 1 || spec.Port > 65535 || spec.RemotePort < 1 || spec.RemotePort > 65535 || spec.RemoteHost == "" {
				return
			}
			dialer := net.Dialer{Timeout: 5 * time.Second}
			c, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(spec.RemoteHost, strconv.Itoa(spec.RemotePort)))
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

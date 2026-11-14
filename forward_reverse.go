package main

// Only an explicitly approved rule may open a local destination. Never trust a
// server-supplied host/port: match the full rule, then dial its stored endpoint.
import (
	"context"
	"fmt"
	"github.com/xtaci/smux"
	"net"
	"time"
)

func (f *forwardManager) remoteRequest(m message) message {
	fail := func(e error) message { return message{Type: "forward_result", Error: e.Error()} }
	st, e := f.mux.OpenStream()
	if e != nil {
		return fail(e)
	}
	defer st.Close()
	abort := context.AfterFunc(f.ctx, func() { st.Close() })
	defer abort()
	st.SetDeadline(time.Now().Add(forwardTimeout))
	m.Type = "forward_control"
	m.Reverse = false
	if e = writeMessage(st, m); e != nil {
		return fail(e)
	}
	r, e := readMessage(st)
	if e != nil {
		return fail(e)
	}
	if r.Type != "forward_result" {
		return fail(fmt.Errorf("invalid forward control response"))
	}
	r.Reverse = true
	return r
}

func (f *forwardManager) handleReverse(st *smux.Stream, m message) {
	defer st.Close()
	if m.Port < 1 || m.Port > 65535 {
		return
	}
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	default:
		return
	}
	f.mu.Lock()
	r := f.rules[forwardKey{m.Port, true}]
	if !m.Reverse || r == nil || !r.reverse || m.RemoteHost != r.remoteHost || m.RemotePort != r.remotePort {
		f.mu.Unlock()
		return
	}
	ctx := r.ctx
	target := loopback(r.remotePort)
	f.mu.Unlock()
	abort := context.AfterFunc(ctx, func() { st.Close() })
	defer abort()
	st.SetDeadline(time.Now().Add(forwardTimeout))
	dialer := net.Dialer{Timeout: 5 * time.Second}
	c, e := dialer.DialContext(ctx, "tcp4", target)
	reply := message{Type: "forward_ready"}
	if e != nil {
		reply.Error = e.Error()
		writeMessage(st, reply)
		return
	}
	defer c.Close()
	if e = writeMessage(st, reply); e != nil {
		return
	}
	st.SetDeadline(time.Time{})
	relayForward(ctx, c, st)
}

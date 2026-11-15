package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	shellmux "github.com/jay-waves/sshore/internal/mux"
	"github.com/xtaci/smux"
)

// Accept both streams before handing ownership to a shell. Every partial
// handshake failure closes the transport here; callers own successful results.
func acceptMux(c io.ReadWriteCloser) (mx *smux.Session, control, terminal *smux.Stream, request message, err error) {
	mx, err = smux.Server(c, muxConfig())
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			mx.Close()
		}
	}()
	_ = mx.SetDeadline(time.Now().Add(5 * time.Second))
	control, err = mx.AcceptStream()
	if err != nil {
		return
	}
	_ = control.SetReadDeadline(time.Now().Add(5 * time.Second))
	request, err = readMessage(control)
	if err != nil {
		return
	}
	_ = control.SetReadDeadline(time.Time{})
	terminal, err = mx.OpenStream()
	if err != nil {
		return
	}
	err = streamMessage(terminal, message{Type: "terminal"})
	_ = mx.SetDeadline(time.Time{})
	return
}

func validateAttach(m message) error {
	if m.Type != "attach" {
		return fmt.Errorf("expected attach request")
	}
	if m.Term == "" || len(m.Term) > 128 || strings.ContainsAny(m.Term, "\x00\r\n\x1b") {
		return fmt.Errorf("invalid TERM")
	}
	return nil
}

type terminalInputLease interface {
	io.Writer
	Stop() error
	Stopped() bool
}

type attachment struct {
	mux               *smux.Session
	control, terminal *smux.Stream
	controlMu         sync.Mutex
	terminalWriteMu   sync.Mutex
	approvalRequests  chan struct{}
	input             terminalInputLease
	muted             atomic.Bool
}

func (p *attachment) controlMessage(m message) error {
	p.controlMu.Lock()
	defer p.controlMu.Unlock()
	return streamMessage(p.control, m)
}
func (p *attachment) output(m message) error {
	p.terminalWriteMu.Lock()
	defer p.terminalWriteMu.Unlock()
	if m.Type == "output" && p.muted.Load() {
		return nil
	}
	return streamMessage(p.terminal, m)
}
func (p *attachment) close() { _ = p.mux.Close() }

// Exit follows output on the terminal stream; let the client consume it before
// transport shutdown, with a bounded wait for an unresponsive peer.
func (p *attachment) finishExit(code int) {
	_ = p.output(message{Type: "exit", Code: code})
	select {
	case <-p.mux.CloseChan():
	case <-time.After(2 * time.Second):
		p.close()
	}
}

func (p *attachment) readInput(owner *shellmux.Lifecycle[*attachment]) {
	for {
		m, err := readMessage(p.terminal)
		if err != nil {
			p.close()
			return
		}
		if owner.Active() != p {
			return
		}
		switch m.Type {
		case "input":
			err = writeFull(p.input, m.Data)
		case "input_barrier":
			err = p.output(message{Type: "input_drained"})
		default:
			err = fmt.Errorf("invalid terminal input")
		}
		if err != nil {
			if !p.input.Stopped() {
				p.close()
			}
			return
		}
	}
}

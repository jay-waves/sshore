package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/xtaci/smux"

	"github.com/jay-waves/sshore/internal/shell"
	"github.com/jay-waves/sshore/internal/tmux"
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

type attachment struct {
	*shell.Attachment
	approvalRequests chan struct{}
}

func newAttachment(mx *smux.Session, control, terminal *smux.Stream) *attachment {
	return &attachment{Attachment: shell.NewAttachment(mx, control, terminal), approvalRequests: make(chan struct{}, 8)}
}
func (p *attachment) controlMessage(m message) error { return p.Control(m.Type, m.Data, m) }
func (p *attachment) output(m message) error         { return p.Output(m.Type, m.Data, m) }
func (p *attachment) readInput(owner *tmux.Lifecycle[*attachment]) {
	p.ReadInput(func() bool { return owner.Active() == p })
}

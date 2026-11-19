package tmux

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/wire"
	"github.com/xtaci/smux"
)

type Input interface {
	io.Writer
	Stopped() bool
}

// Attachment owns the I/O of one shell attachment, not shell authorization.
// ReadInput borrows the host-owned input lease for one reader.
type Attachment struct {
	mux                 *smux.Session
	control, terminal   *smux.Stream
	controlMu, outputMu sync.Mutex
}

func NewAttachment(mx *smux.Session, control, terminal *smux.Stream) *Attachment {
	return &Attachment{mux: mx, control: control, terminal: terminal}
}
func (a *Attachment) OpenStream() (*smux.Stream, error) { return a.mux.OpenStream() }
func (a *Attachment) Done() <-chan struct{}             { return a.mux.CloseChan() }
func (a *Attachment) Close()                            { _ = a.mux.Close() }
func send(stream *smux.Stream, kind string, data []byte, metadata any) error {
	_ = stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := wire.WriteMessage(stream, kind, data, metadata)
	_ = stream.SetWriteDeadline(time.Time{})
	return err
}
func (a *Attachment) Control(kind string, data []byte, metadata any) error {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	return send(a.control, kind, data, metadata)
}
func (a *Attachment) Output(kind string, data []byte, metadata any) error {
	a.outputMu.Lock()
	defer a.outputMu.Unlock()
	return send(a.terminal, kind, data, metadata)
}

type terminalMessage struct {
	Reason string `json:"reason,omitempty"`
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
	Code   int    `json:"code,omitempty"`
	Data   []byte `json:"-"`
}

func (m *terminalMessage) FrameType() string                 { return m.Type }
func (m *terminalMessage) SetFrame(kind string, data []byte) { m.Type, m.Data = kind, data }

// FinishExit follows terminal output, allowing bounded consumption before close.
func (a *Attachment) FinishExit(code int, reason string) {
	_ = a.Output("exit", nil, terminalMessage{Type: "exit", Code: code, Reason: reason})
	select {
	case <-a.mux.CloseChan():
	case <-time.After(2 * time.Second):
		a.Close()
	}
}

func (a *Attachment) ReadInput(input Input, active func() bool) {
	for {
		var m terminalMessage
		err := wire.ReadMessage(a.terminal, &m)
		if err != nil {
			a.Close()
			return
		}
		if !active() {
			return
		}
		switch m.Type {
		case "input":
			err = wire.WriteFull(input, m.Data)
		case "input_barrier":
			err = a.Output("input_drained", nil, terminalMessage{Type: "input_drained"})
		default:
			err = fmt.Errorf("invalid terminal input")
		}
		if err != nil {
			if !input.Stopped() {
				a.Close()
			}
			return
		}
	}
}

type controlStream interface {
	io.ReadWriter
	SetDeadline(time.Time) error
}

// Detach completes the detach/confirmation/bye handshake on an exclusively owned
// control stream. Confirmation runs before bye, so callers can retain the name
// even if the final acknowledgement fails.
func Detach(control controlStream, confirmed ...func(string)) error {
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	defer control.SetDeadline(time.Time{})
	if err := wire.WriteMessage(control, "detach", nil, terminalMessage{Type: "detach"}); err != nil {
		return err
	}
	m, err := expectDetachMessage(control, "detached")
	if err != nil {
		return err
	}
	for _, confirm := range confirmed {
		confirm(m.Name)
	}
	return wire.WriteMessage(control, "bye", nil, terminalMessage{Type: "bye"})
}

func expectDetachMessage(r io.Reader, expected string) (terminalMessage, error) {
	var m terminalMessage
	if err := wire.ReadMessage(r, &m); err != nil {
		return m, err
	}
	if m.Type != expected {
		return m, fmt.Errorf("expected %s in detach handshake, received %s", expected, m.Type)
	}
	return m, nil
}

// FinishDetach confirms the handoff after the host releases the input lease.
// Only the control loop may call it, since it consumes the final acknowledgement.
func (a *Attachment) FinishDetach(name string) error {
	defer a.Close()
	if err := a.Control("detached", nil, terminalMessage{Type: "detached", Name: name}); err != nil {
		return err
	}
	_ = a.control.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := expectDetachMessage(a.control, "bye")
	return err
}

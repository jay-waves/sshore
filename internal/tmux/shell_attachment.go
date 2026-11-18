package tmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
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
	muted               atomic.Bool
}

func NewAttachment(mx *smux.Session, control, terminal *smux.Stream) *Attachment {
	return &Attachment{mux: mx, control: control, terminal: terminal}
}
func (a *Attachment) OpenStream() (*smux.Stream, error) { return a.mux.OpenStream() }
func (a *Attachment) Done() <-chan struct{}             { return a.mux.CloseChan() }
func (a *Attachment) Muted() bool                       { return a.muted.Load() }
func (a *Attachment) Mute(muted bool) bool              { return a.muted.Swap(muted) }
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
	if kind == "output" && a.muted.Load() {
		return nil
	}
	return send(a.terminal, kind, data, metadata)
}

type terminalMessage struct {
	Reason string `json:"reason,omitempty"`
	Type   string `json:"type"`
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
type detachMessage struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// Detach completes the detach/confirmation/bye handshake on an exclusively owned
// control stream. Confirmation runs before bye, so callers can retain the name
// even if the final acknowledgement fails.
func Detach(control controlStream, confirmed ...func(string)) error {
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	defer control.SetDeadline(time.Time{})
	if err := writeControl(control, detachMessage{Type: "detach"}); err != nil {
		return err
	}
	kind, body, err := wire.ReadFrame(control)
	if err != nil {
		return err
	}
	var m detachMessage
	if kind != 0 {
		return fmt.Errorf("unexpected binary detach response")
	}
	if err = json.Unmarshal(body, &m); err != nil {
		return err
	}
	if m.Type != "detached" {
		return fmt.Errorf("unexpected detach response: %s", m.Type)
	}
	for _, confirm := range confirmed {
		confirm(m.Name)
	}
	return writeControl(control, detachMessage{Type: "bye"})
}
func writeControl(w io.Writer, m detachMessage) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return wire.WriteFrame(w, 0, body)
}

// FinishDetach confirms the handoff after the host releases the input lease.
// Only the control loop may call it, since it consumes the final acknowledgement.
func (a *Attachment) FinishDetach(name string) {
	defer a.Close()
	if err := a.Control("detached", nil, detachMessage{Type: "detached", Name: name}); err != nil {
		return
	}
	_ = a.control.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, _ = wire.ReadFrame(a.control)
}

var ErrExited = errors.New("session is exiting")
var ErrAttached = errors.New("session already attached")
var ErrMissingAttachment = errors.New("missing attachment")

// Lifecycle owns exclusive attachments. A is an attachment identity, normally a
// pointer. Its zero value is detached. Prepare must not call back into Lifecycle.
type Lifecycle[A comparable] struct {
	mu     sync.Mutex
	active A
	exited bool
}

func (s *Lifecycle[A]) Active() A    { s.mu.Lock(); defer s.mu.Unlock(); return s.active }
func (s *Lifecycle[A]) Exited() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.exited }
func (s *Lifecycle[A]) Attach(a A, prepare func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero A
	if a == zero {
		return ErrMissingAttachment
	}
	if s.exited {
		return ErrExited
	}
	if s.active != zero {
		return ErrAttached
	}
	if err := prepare(); err != nil {
		return err
	}
	s.active = a
	return nil
}

// Detach preserves the terminal; stale identities cannot detach another client.
func (s *Lifecycle[A]) Detach(a A) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != a {
		return false
	}
	var zero A
	if a == zero {
		return false
	}
	s.active = zero
	return true
}

// EndIfAttached prevents a failed input handoff from admitting another writer.
func (s *Lifecycle[A]) EndIfAttached(a A) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero A
	if a == zero || s.active != a {
		return false
	}
	s.active = zero
	s.exited = true
	return true
}
func (s *Lifecycle[A]) End() A {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.active
	var zero A
	s.active = zero
	s.exited = true
	return a
}

package shell

import (
	"fmt"
	"github.com/jay-waves/sshore/internal/transport"
	"github.com/xtaci/smux"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type Input interface {
	io.Writer
	Stop() error
	Stopped() bool
}

// Attachment owns the I/O of one shell attachment, not shell authorization.
// The host installs Input before starting readers and stops it before handoff.
type Attachment struct {
	mux                 *smux.Session
	control, terminal   *smux.Stream
	controlMu, outputMu sync.Mutex
	input               Input
	muted               atomic.Bool
}

func NewAttachment(mx *smux.Session, control, terminal *smux.Stream) *Attachment {
	return &Attachment{mux: mx, control: control, terminal: terminal}
}
func (a *Attachment) Mux() *smux.Session          { return a.mux }
func (a *Attachment) ControlStream() *smux.Stream { return a.control }
func (a *Attachment) Input() Input                { return a.input }
func (a *Attachment) SetInput(input Input)        { a.input = input }
func (a *Attachment) Muted() bool                 { return a.muted.Load() }
func (a *Attachment) Mute(muted bool) bool        { return a.muted.Swap(muted) }
func (a *Attachment) Close()                      { _ = a.mux.Close() }
func send(stream *smux.Stream, kind string, data []byte, metadata any) error {
	_ = stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := transport.WriteMessage(stream, kind, data, metadata)
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
	Type string `json:"type"`
	Code int    `json:"code,omitempty"`
	Data []byte `json:"-"`
}

func (m *terminalMessage) FrameType() string                 { return m.Type }
func (m *terminalMessage) SetFrame(kind string, data []byte) { m.Type, m.Data = kind, data }

// FinishExit follows terminal output, allowing bounded consumption before close.
func (a *Attachment) FinishExit(code int) {
	_ = a.Output("exit", nil, terminalMessage{Type: "exit", Code: code})
	select {
	case <-a.mux.CloseChan():
	case <-time.After(2 * time.Second):
		a.Close()
	}
}

func (a *Attachment) ReadInput(active func() bool) {
	for {
		var m terminalMessage
		err := transport.ReadMessage(a.terminal, &m)
		if err != nil {
			a.Close()
			return
		}
		if !active() {
			return
		}
		switch m.Type {
		case "input":
			err = transport.WriteFull(a.input, m.Data)
		case "input_barrier":
			err = a.Output("input_drained", nil, terminalMessage{Type: "input_drained"})
		default:
			err = fmt.Errorf("invalid terminal input")
		}
		if err != nil {
			if !a.input.Stopped() {
				a.Close()
			}
			return
		}
	}
}

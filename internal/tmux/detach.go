package tmux

import (
	"encoding/json"
	"fmt"
	"github.com/jay-waves/sshore/internal/transport"
	"io"
	"time"
)

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
	kind, body, err := transport.ReadFrame(control)
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
	return transport.WriteFrame(w, 0, body)
}

// FinishDetach lets the peer consume the confirmation before closing transport.
// The caller has already relinquished attachment ownership and serializes notify
// with other control writes. No second reader may consume the acknowledgement.
func FinishDetach(control interface {
	io.Reader
	SetReadDeadline(time.Time) error
}, notify func() error, closeTransport func()) {
	defer closeTransport()
	_ = notify()
	_ = control.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, _ = transport.ReadFrame(control)
}

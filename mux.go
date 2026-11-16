package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/xtaci/smux"

	"github.com/jay-waves/sshore/internal/shell"
	"github.com/jay-waves/sshore/internal/tmux"
	transportio "github.com/jay-waves/sshore/internal/transport"
)

// Logical control, terminal and file streams are application-level
// smux v2 streams over one SSH connection, not native OpenSSH channels. The
// handshake requires the exact full sshore release, including PATCH; there is no
// separate sshore protocol compatibility counter. Running daemons cannot upgrade
// in place. Each stream has a 256 KiB receive window, a bounded shared receive
// budget and a 16 KiB maximum frame size. These isolate stalled streams without
// guaranteeing real-time terminal priority: TCP head-of-line blocking still applies.
// Keepalives detect silent transport loss (10s interval, 60s timeout); detection
// releases the attachment while preserving the shell.

func clientMux(c io.ReadWriteCloser) (*smux.Session, error) {
	if err := writeMessage(c, message{Type: "mux", SoftwareVersion: version}); err != nil {
		return nil, err
	}
	m, err := readMessage(c)
	if err != nil {
		return nil, fmt.Errorf("SSH shell handshake failed: %w", err)
	}
	if err := checkPeerVersion(m.SoftwareVersion, "remote shell daemon"); err != nil {
		return nil, err
	}
	if m.Type != "mux_ready" {
		return nil, fmt.Errorf("incompatible sshore server: %s %s", m.Type, m.Error)
	}
	return smux.Client(c, transportio.MuxConfig())
}
func streamMessage(s *smux.Stream, m message) error {
	_ = s.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := writeMessage(s, m)
	_ = s.SetWriteDeadline(time.Time{})
	return err
}

// Check the running process, not just the executable currently on disk.
func checkPeerVersion(peer, role string) error {
	if sameRelease(version, peer) {
		return nil
	}
	if peer == "" {
		peer = "unknown (older build)"
	}
	return fmt.Errorf("sshore version mismatch: this process=%s, %s=%s; all managed processes must use the same full version. Reconnect through sshore setup to update or enter a recovery shell. Existing shell daemons keep their old version after an upgrade; use a new shell name, or use matching old client and remote binaries (--server PATH) to finish work and exit that shell", version, role, peer)
}

// Accept both streams before handing ownership to a shell. Every partial
// handshake failure closes the transport here; callers own successful results.
func acceptMux(c io.ReadWriteCloser) (mx *smux.Session, control, terminal *smux.Stream, request message, err error) {
	mx, err = smux.Server(c, transportio.MuxConfig())
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
	if (m.NoLogin || m.NoProfile) && m.Action != "" {
		return fmt.Errorf("startup options require a new shell")
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

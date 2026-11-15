package main

// Logical control, terminal and file streams are application-level
// smux v2 streams over one SSH connection, not native OpenSSH channels. The
// handshake requires the exact full sshore release, including PATCH; there is no
// separate sshore protocol compatibility counter. Running daemons cannot upgrade
// in place. Each stream has a 256 KiB receive window, a bounded shared receive
// budget and a 16 KiB maximum frame size. These isolate stalled streams without
// guaranteeing real-time terminal priority: TCP head-of-line blocking still applies.
// Keepalives detect silent transport loss (10s interval, 60s timeout); detection
// releases the attachment while preserving the shell.

import (
	"fmt"
	"io"
	"time"

	"github.com/xtaci/smux"

	"github.com/jay-waves/sshore/internal/transport"
)

// The module's v1 release supports wire protocols 1 and 2. Explicitly select
// protocol 2 for independent stream receive windows.
func muxConfig() *smux.Config { return transport.MuxConfig() }

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
	return smux.Client(c, muxConfig())
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

type pipeTransport = transport.Pipe

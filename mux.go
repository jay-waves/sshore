package main

// Logical control, terminal and file streams are application-level
// smux v2 streams over one SSH connection, not native OpenSSH channels. The
// handshake requires the exact full sshore release, including PATCH; there is no
// separate sshore protocol compatibility counter. Running daemons cannot upgrade
// in place. Each stream has a 256 KiB receive window, a bounded shared receive
// budget and a 16 KiB maximum frame size. These isolate stalled streams without
// guaranteeing real-time terminal priority: TCP head-of-line blocking still applies.
// Keepalives detect silent transport loss (10s interval, 60s timeout); detection
// ends an attached session rather than granting an implicit detach.

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

// The module's v1 release supports wire protocols 1 and 2. Explicitly select
// protocol 2 for independent stream receive windows.
func muxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.MaxFrameSize = 16 * 1024
	c.MaxStreamBuffer = 256 * 1024
	// Keep room for terminal, files and control requests. Independent forwards
	// and mounts use separate SSH connections and do not consume this budget.
	// Stalled streams must not exhaust the shared receive budget and block input.
	c.MaxReceiveBuffer = (maxApprovedStreams + 12) * c.MaxStreamBuffer
	c.KeepAliveInterval = 10 * time.Second
	c.KeepAliveTimeout = 60 * time.Second
	return c
}

type pipeTransport struct {
	io.Reader
	io.Writer
	closers []io.Closer
	once    sync.Once
}

func (p *pipeTransport) Close() error {
	p.once.Do(func() {
		for _, c := range p.closers {
			_ = c.Close()
		}
	})
	return nil
}

func clientMux(c io.ReadWriteCloser) (*smux.Session, error) {
	if err := writeMessage(c, message{Type: "mux", SoftwareVersion: version}); err != nil {
		return nil, err
	}
	m, err := readMessage(c)
	if err != nil {
		return nil, fmt.Errorf("SSH session handshake failed: %w", err)
	}
	if err := checkPeerVersion(m.SoftwareVersion, "remote session daemon"); err != nil {
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
	if compatibleVersions(version, peer) {
		return nil
	}
	if peer == "" {
		peer = "unknown (older build)"
	}
	return fmt.Errorf("sshore version mismatch: this process=%s, %s=%s; all managed processes must use the same full version. Reconnect through sshore setup to update or enter a recovery shell. Existing session daemons keep their old version after an upgrade; use a new session name, or use matching old client and remote binaries (--server PATH) to finish work and exit that shell", version, role, peer)
}

// Parse stable release labels for upgrade direction; this is not a compatibility series.
func releaseSeries(v string) (string, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return "", false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return "", false
			}
		}
	}
	return parts[0] + "." + parts[1], true
}

func compatibleVersions(local, peer string) bool {
	return local != "" && local == peer
}

// Compare numeric release components without integer overflow. Custom labels
// cannot be ordered and must be handled manually when they differ.
func compareVersions(a, b string) (int, bool) {
	if a == b && a != "" {
		return 0, true
	}
	if _, ok := releaseSeries(a); !ok {
		return 0, false
	}
	if _, ok := releaseSeries(b); !ok {
		return 0, false
	}
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range aa {
		if len(aa[i]) < len(bb[i]) {
			return -1, true
		}
		if len(aa[i]) > len(bb[i]) {
			return 1, true
		}
		if aa[i] < bb[i] {
			return -1, true
		}
		if aa[i] > bb[i] {
			return 1, true
		}
	}
	return 0, true
}

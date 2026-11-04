package main

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

const maxTransfers = 2

// The module's v1 release supports wire protocols 1 and 2. Explicitly select
// protocol 2 for independent stream receive windows.
func muxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.MaxFrameSize = 16 * 1024
	c.MaxStreamBuffer = 256 * 1024
	// Keep room for every permitted stream. A stalled file must not exhaust the
	// connection's shared receive budget and stall terminal/control traffic.
	c.MaxReceiveBuffer = 2 * 1024 * 1024
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
	if err := writeMessage(c, message{Type: "mux", Version: protocolVersion}); err != nil {
		return nil, err
	}
	m, err := readMessage(c)
	if err != nil {
		return nil, fmt.Errorf("SSH handshake failed (install matching sshore on the remote PATH): %w", err)
	}
	if m.Type != "mux_ready" || m.Version != protocolVersion {
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

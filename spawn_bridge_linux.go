package main

import (
	"fmt"
	"github.com/xtaci/smux"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// The original SSH connection is the lifetime lease; individual attachments
// can end without closing it. Its first stream carries the initial attachment.
func bridgeSpawn(path string, lease net.Conn, readyMode string) error {
	if err := writeMessage(lease, message{Type: "spawn_owner", SoftwareVersion: version}); err != nil {
		return err
	}
	reply, err := readMessage(lease)
	if err != nil {
		return err
	}
	if reply.Type != "spawn_owned" {
		return fmt.Errorf("spawn lease rejected: %s", reply.Error)
	}
	if err = writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: readyMode}); err != nil {
		return err
	}
	transport := &pipeTransport{Reader: os.Stdin, Writer: os.Stdout}
	first, err := readMessage(transport)
	if err != nil {
		return err
	}
	if first.Type != "mux" || checkPeerVersion(first.SoftwareVersion, "client") != nil {
		return fmt.Errorf("invalid spawn transport")
	}
	if err = writeMessage(transport, message{Type: "mux_ready", SoftwareVersion: version}); err != nil {
		return err
	}
	mx, err := smux.Server(transport, muxConfig())
	if err != nil {
		return err
	}
	defer mx.Close()
	go func() { _, _ = io.Copy(io.Discard, lease); mx.Close() }()
	slots := make(chan struct{}, 8)
	var handlers sync.WaitGroup
	defer handlers.Wait()
	for {
		stream, err := mx.AcceptStream()
		if err != nil {
			return nil
		}
		select {
		case slots <- struct{}{}:
		default:
			stream.Close()
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-slots }()
			defer stream.Close()
			c, err := net.DialTimeout("unix", path, time.Second)
			if err != nil {
				return
			}
			defer c.Close()
			proxySpawn(stream, c)
		}()
	}
}

func (s *shell) serveSpawnOwner(c net.Conn, first message) {
	s.mu.Lock()
	if !s.temporary || s.spawnOwned || checkPeerVersion(first.SoftwareVersion, "client") != nil {
		s.mu.Unlock()
		_ = writeMessage(c, message{Type: "error", Error: "spawn lifetime already owned or unavailable"})
		return
	}
	s.spawnOwned = true
	instance := s.instance
	s.mu.Unlock()
	defer func() { _ = s.cancel(instance) }()
	if writeMessage(c, message{Type: "spawn_owned"}) != nil {
		return
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-s.done:
			c.Close()
		case <-done:
		}
	}()
	_, _ = io.Copy(io.Discard, c)
}

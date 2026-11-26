package main

import (
	"crypto/rand"
	"fmt"
	"net"
	"sync"
	"time"
)

// Terminal endpoints are authenticated by the daemon before this handshake.
// A short-lived, single-use token binds the raw endpoint to one attachment.
type terminalEndpoints struct {
	mu      sync.Mutex
	pending map[string]*terminalEndpoint
}
type terminalEndpoint struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *terminalEndpoint) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}
func (r *terminalEndpoints) serve(c net.Conn, request message) error {
	if err := checkPeerVersion(request.SoftwareVersion, "terminal client"); err != nil {
		return err
	}
	endpoint := &terminalEndpoint{Conn: c, done: make(chan struct{})}
	defer endpoint.Close()
	token := rand.Text()
	r.mu.Lock()
	if len(r.pending) >= 8 {
		r.mu.Unlock()
		return fmt.Errorf("too many pending terminal endpoints")
	}
	if r.pending == nil {
		r.pending = make(map[string]*terminalEndpoint)
	}
	r.pending[token] = endpoint
	r.mu.Unlock()
	defer r.take(token)
	timer := time.AfterFunc(5*time.Second, func() {
		if unused := r.take(token); unused != nil {
			_ = unused.Close()
		}
	})
	defer timer.Stop()
	if err := writeMessage(c, message{Type: "terminal_ready", SoftwareVersion: version, Token: token}); err != nil {
		return err
	}
	<-endpoint.done
	return nil
}
func (r *terminalEndpoints) take(token string) *terminalEndpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.pending[token]
	delete(r.pending, token)
	return c
}

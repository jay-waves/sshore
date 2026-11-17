package ssh

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

type forwarding struct {
	mu          sync.Mutex
	closing     bool
	listeners   []net.Listener
	connections []net.Conn
	workers     sync.WaitGroup
}

func (f *forwarding) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closing = true
	for _, l := range f.listeners {
		l.Close()
	}
	for _, c := range f.connections {
		c.Close()
	}
}
func (f *forwarding) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closing {
		c.Close()
		return false
	}
	f.connections = append(f.connections, c)
	return true
}
func (f *forwarding) untrack(c net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, v := range f.connections {
		if v == c {
			f.connections = append(f.connections[:i], f.connections[i+1:]...)
			return
		}
	}
}

// Forward is one product-owned loopback rule, independent of SSH configuration.
type Forward struct {
	Port, Target int
	Reverse      bool
}

// Close joins all listeners and sockets installed on this client.
func (c *Client) StartForwarding() error {
	rule := c.config.options.Forward
	if rule == nil {
		return nil
	}
	if rule.Port < 1 || rule.Port > 65535 || rule.Target < 1 || rule.Target > 65535 {
		return fmt.Errorf("invalid forward ports")
	}
	listen := net.JoinHostPort("127.0.0.1", strconv.Itoa(rule.Port))
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(rule.Target))
	var listener net.Listener
	var err error
	if rule.Reverse {
		listener, err = c.Listen("tcp4", listen)
	} else {
		listener, err = net.Listen("tcp4", listen)
	}
	if err != nil {
		return fmt.Errorf("forward %s: %w", listen, err)
	}
	c.forwards.mu.Lock()
	if c.forwards.closing {
		c.forwards.mu.Unlock()
		listener.Close()
		return net.ErrClosed
	}
	c.forwards.listeners = append(c.forwards.listeners, listener)
	c.forwards.workers.Add(1)
	c.forwards.mu.Unlock()
	go c.acceptForward(listener, target, rule.Reverse)
	return nil
}

func (c *Client) dialForwardTarget(target string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() {
		select {
		case <-c.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return (&net.Dialer{}).DialContext(ctx, c.config.network, target)
}

func (c *Client) acceptForward(listener net.Listener, target string, reverse bool) {
	defer c.forwards.workers.Done()
	for {
		incoming, err := listener.Accept()
		if err != nil {
			return
		}
		if !c.forwards.track(incoming) {
			return
		}
		c.forwards.mu.Lock()
		if c.forwards.closing {
			c.forwards.mu.Unlock()
			incoming.Close()
			c.forwards.untrack(incoming)
			return
		}
		c.forwards.workers.Add(1)
		c.forwards.mu.Unlock()
		go func() {
			defer c.forwards.workers.Done()
			defer incoming.Close()
			defer c.forwards.untrack(incoming)
			var outgoing net.Conn
			var err error
			if reverse {
				outgoing, err = c.dialForwardTarget(target)
			} else {
				outgoing, err = c.DialContext(context.Background(), "tcp", target)
			}
			if err != nil {
				return
			}
			if !c.forwards.track(outgoing) {
				return
			}
			defer c.forwards.untrack(outgoing)
			defer outgoing.Close()
			var copyWG sync.WaitGroup
			copyWG.Add(1)
			copyTo := func(dst, src net.Conn) {
				io.Copy(dst, src)
				if w, ok := dst.(interface{ CloseWrite() error }); ok {
					w.CloseWrite()
				} else {
					dst.Close()
				}
			}
			go func() { defer copyWG.Done(); copyTo(outgoing, incoming) }()
			copyTo(incoming, outgoing)
			copyWG.Wait()
		}()
	}
}

package ssh

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
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

// StartForwarding installs the rules already evaluated by ssh -G. The caller
// owns this Client and must Close it to join all listeners and forwarded sockets.
func (c *Client) StartForwarding() error {
	if c.config.enabled("clearallforwardings") {
		return nil
	}
	for _, kind := range []string{"localforward", "remoteforward", "dynamicforward"} {
		for _, rule := range c.config.values[kind] {
			fields := strings.Fields(rule)
			dynamic := kind == "dynamicforward"
			if len(fields) != 2 && !dynamic || dynamic && len(fields) != 1 {
				return fmt.Errorf("unsupported %s rule %q", kind, rule)
			}
			listen, err := forwardAddress(fields[0], true)
			if err != nil {
				return err
			}
			target := ""
			if !dynamic {
				target, err = forwardAddress(fields[1], false)
				if err != nil {
					return err
				}
			}
			var listener net.Listener
			if kind == "remoteforward" {
				listener, err = c.Listen("tcp", listen)
			} else {
				listener, err = net.Listen("tcp", listen)
			}
			if err != nil {
				return fmt.Errorf("%s %s: %w", kind, listen, err)
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
			go c.acceptForward(listener, target, kind)
		}
	}
	return nil
}
func forwardAddress(value string, listen bool) (string, error) {
	if strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("Unix socket forwarding is not supported")
	}
	if _, err := strconv.Atoi(value); err == nil {
		if !listen {
			return "", fmt.Errorf("forward target needs a hostname")
		}
		return net.JoinHostPort("127.0.0.1", value), nil
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("invalid forward address %q", value)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("invalid forward port")
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if host == "*" {
		host = ""
	}
	return net.JoinHostPort(host, port), nil
}
func (c *Client) acceptForward(listener net.Listener, target, kind string) {
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
			destination := target
			if kind == "dynamicforward" {
				incoming.SetDeadline(time.Now().Add(15 * time.Second))
				var err error
				destination, err = socksDestination(incoming)
				if err != nil {
					return
				}
			}
			var outgoing net.Conn
			var err error
			if kind == "remoteforward" {
				outgoing, err = net.DialTimeout("tcp", destination, 15*time.Second)
			} else {
				outgoing, err = c.DialContext(context.Background(), "tcp", destination)
			}
			if err != nil {
				if kind == "dynamicforward" {
					incoming.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
				}
				return
			}
			if !c.forwards.track(outgoing) {
				return
			}
			defer c.forwards.untrack(outgoing)
			defer outgoing.Close()
			if kind == "dynamicforward" {
				if _, err = incoming.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				incoming.SetDeadline(time.Time{})
			}
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
func socksDestination(c net.Conn) (string, error) {
	var header [2]byte
	if _, err := io.ReadFull(c, header[:]); err != nil {
		return "", err
	}
	if header[0] != 5 {
		return "", fmt.Errorf("SOCKS5 required")
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	ok := false
	for _, method := range methods {
		if method == 0 {
			ok = true
		}
	}
	if !ok {
		c.Write([]byte{5, 255})
		return "", fmt.Errorf("SOCKS authentication unsupported")
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return "", err
	}
	var request [4]byte
	if _, err := io.ReadFull(c, request[:]); err != nil {
		return "", err
	}
	if request[0] != 5 || request[1] != 1 || request[2] != 0 {
		return "", fmt.Errorf("SOCKS CONNECT required")
	}
	var host string
	switch request[3] {
	case 1:
		data := make([]byte, 4)
		if _, err := io.ReadFull(c, data); err != nil {
			return "", err
		}
		host = net.IP(data).String()
	case 4:
		data := make([]byte, 16)
		if _, err := io.ReadFull(c, data); err != nil {
			return "", err
		}
		host = net.IP(data).String()
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return "", err
		}
		data := make([]byte, int(n[0]))
		if _, err := io.ReadFull(c, data); err != nil {
			return "", err
		}
		host = string(data)
	default:
		return "", fmt.Errorf("unsupported SOCKS address")
	}
	var port [2]byte
	if _, err := io.ReadFull(c, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

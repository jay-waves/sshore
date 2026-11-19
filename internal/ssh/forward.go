package ssh

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/jay-waves/sshore/internal/wire"
)

// Forward is one product-owned loopback rule, independent of SSH configuration.
type Forward struct {
	Port, Target int
	Reverse      bool
}

func (c *Client) startForwarding(rule Forward) error {
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
	c.forwardMu.Lock()
	defer c.forwardMu.Unlock()
	select {
	case <-c.done:
		listener.Close()
		return net.ErrClosed
	default:
	}
	dial := func(ctx context.Context) (net.Conn, error) {
		return c.DialContext(ctx, "tcp", target)
	}
	if rule.Reverse {
		dialer := net.Dialer{Timeout: 15 * time.Second}
		dial = func(ctx context.Context) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", target)
		}
	}
	c.forward = wire.StartForwardListener(listener, dial)
	return nil
}

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/jay-waves/sshore/internal/wire"
)

type DialFunc func(context.Context, string) (io.ReadWriteCloser, error)

// Client routes each request to its host; session references survive client restarts.
type Client struct {
	Version     string
	Dial        DialFunc
	mu          sync.Mutex
	connections map[string]*clientPool
	closed      bool
}
type clientConnection struct {
	ctx    context.Context
	cancel context.CancelFunc
	conn   io.ReadWriteCloser
	idle   bool // Protected by Client.mu.
}

const maxHostConnections = 8

type clientPool struct {
	connections []*clientConnection
	changed     chan struct{}
}

// Each transport carries one protocol exchange at a time. Concurrent calls grow
// a host's pool on demand instead of waiting behind an unrelated long read.
func (c *Client) acquire(ctx context.Context, host string) (*clientConnection, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, fmt.Errorf("MCP client closed")
		}
		if c.connections == nil {
			c.connections = make(map[string]*clientPool)
		}
		pool := c.connections[host]
		if pool == nil {
			pool = &clientPool{changed: make(chan struct{})}
			c.connections[host] = pool
		}
		for _, entry := range pool.connections {
			if entry.idle {
				entry.idle = false
				c.mu.Unlock()
				return entry, nil
			}
		}
		if len(pool.connections) < maxHostConnections {
			lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
			entry := &clientConnection{ctx: lifetime, cancel: cancel}
			pool.connections = append(pool.connections, entry)
			c.mu.Unlock()
			return entry, nil
		}
		changed := pool.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (c *Client) release(host string, entry *clientConnection, reuse bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	pool := c.connections[host]
	if reuse {
		entry.idle = true
	} else {
		i := slices.Index(pool.connections, entry)
		pool.connections = slices.Delete(pool.connections, i, i+1)
	}
	close(pool.changed)
	if len(pool.connections) == 0 {
		delete(c.connections, host)
	} else {
		pool.changed = make(chan struct{})
	}
}

// Shutdown releases transports without ending persistent remote sessions.
func (c *Client) Shutdown() {
	c.mu.Lock()
	c.closed = true
	pools := c.connections
	c.connections = nil
	c.mu.Unlock()
	// A closed client can neither publish a stream nor change a pool.
	// Cancel every slot before waiting for any transport to close.
	for _, pool := range pools {
		close(pool.changed)
		for _, entry := range pool.connections {
			entry.cancel()
		}
	}
	for _, pool := range pools {
		for _, entry := range pool.connections {
			if entry.conn != nil {
				_ = entry.conn.Close()
			}
		}
	}
}

type sessionReference struct {
	Host string `json:"host"`
	ID   string `json:"id"`
}

func encodeReference(host, id string) (string, error) {
	if err := validateHost(host); err != nil {
		return "", err
	}
	if id == "" || len(id) > 256 {
		return "", fmt.Errorf("invalid session ID")
	}
	b, err := json.Marshal(sessionReference{Host: host, ID: id})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func decodeReference(id string) (sessionReference, error) {
	var ref sessionReference
	if len(id) > 2048 {
		return ref, fmt.Errorf("invalid session reference")
	}
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return ref, fmt.Errorf("invalid session reference")
	}
	if err = json.Unmarshal(b, &ref); err != nil || ref.ID == "" || len(ref.ID) > 256 || validateHost(ref.Host) != nil {
		return sessionReference{}, fmt.Errorf("invalid session reference")
	}
	return ref, nil
}

func (c *Client) call(ctx context.Context, host string, req Request) (Response, error) {
	if err := validateHost(host); err != nil {
		return Response{}, err
	}
	if c.Dial == nil {
		return Response{}, fmt.Errorf("transport unavailable")
	}
	entry, err := c.acquire(ctx, host)
	if err != nil {
		return Response{}, err
	}
	conn := entry.conn
	reuse := false
	stop := context.AfterFunc(ctx, entry.cancel)
	defer func() {
		keep := stop() && reuse && ctx.Err() == nil
		if !keep {
			entry.cancel()
			if conn != nil {
				_ = conn.Close()
			}
		}
		c.release(host, entry, keep)
	}()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if err := entry.ctx.Err(); err != nil {
		return Response{}, err
	}
	if conn == nil {
		conn, err = c.Dial(entry.ctx, host)
		if err != nil {
			return Response{}, err
		}
		if conn == nil {
			return Response{}, fmt.Errorf("transport unavailable")
		}
		conn = wire.OwnTransport(entry.ctx, conn, nil)

		// Publish under the same lock that detaches pools during shutdown.
		c.mu.Lock()
		requestErr := ctx.Err()
		closed := c.closed || entry.ctx.Err() != nil
		if requestErr == nil && !closed {
			entry.conn = conn
		}
		c.mu.Unlock()
		if requestErr != nil {
			return Response{}, requestErr
		}
		if closed {
			return Response{}, fmt.Errorf("MCP client closed")
		}
	}
	req.Version = c.Version
	if err = WriteRequest(conn, req); err != nil {
		return Response{}, fmt.Errorf("send request: %w", err)
	}
	res, err := ReadResponse(conn)
	if err != nil {
		return Response{}, fmt.Errorf("response unavailable; operation outcome may be unknown: %w", err)
	}
	if res.Version != c.Version {
		return Response{}, fmt.Errorf("remote release mismatch")
	}
	reuse = true
	if res.Error != "" {
		return Response{}, fmt.Errorf("remote: %s", res.Error)
	}
	return res, nil
}
func (c *Client) Open(ctx context.Context, in OpenInput) (Session, error) {
	r, err := c.call(ctx, in.Host, Request{Method: "open", Open: in})
	if err != nil {
		return Session{}, err
	}
	if r.Session == nil {
		return Session{}, fmt.Errorf("invalid response")
	}
	s := *r.Session
	s.ID, err = encodeReference(in.Host, s.ID)
	s.Host = in.Host
	return s, err
}
func (c *Client) List(ctx context.Context, in OpenInput) (Sessions, error) {
	r, err := c.call(ctx, in.Host, Request{Method: "list", Open: in})
	if err != nil {
		return Sessions{}, err
	}
	if r.Sessions == nil {
		return Sessions{}, fmt.Errorf("invalid response")
	}
	for i := range r.Sessions.Sessions {
		s := &r.Sessions.Sessions[i]
		s.ID, err = encodeReference(in.Host, s.ID)
		if err != nil {
			return Sessions{}, err
		}
		s.Host = in.Host
	}
	return *r.Sessions, nil
}
func (c *Client) Execute(ctx context.Context, in ExecuteInput) (Execution, error) {
	ref, err := decodeReference(in.SessionID)
	if err != nil {
		return Execution{}, err
	}
	in.SessionID = ref.ID
	r, err := c.call(ctx, ref.Host, Request{Method: "execute", Execute: in})
	if err != nil {
		return Execution{}, err
	}
	if r.Execution == nil {
		return Execution{}, fmt.Errorf("invalid response")
	}
	return *r.Execution, nil
}
func (c *Client) Read(ctx context.Context, in ReadInput) (Output, error) {
	ref, err := decodeReference(in.SessionID)
	if err != nil {
		return Output{}, err
	}
	in.SessionID = ref.ID
	r, err := c.call(ctx, ref.Host, Request{Method: "read", Read: in})
	if err != nil {
		return Output{}, err
	}
	if r.Output == nil {
		return Output{}, fmt.Errorf("invalid response")
	}
	return *r.Output, nil
}
func (c *Client) Close(ctx context.Context, in SessionInput) (Closed, error) {
	ref, err := decodeReference(in.SessionID)
	if err != nil {
		return Closed{}, err
	}
	in.SessionID = ref.ID
	r, err := c.call(ctx, ref.Host, Request{Method: "close", Session: in})
	if err != nil {
		return Closed{}, err
	}
	if r.Closed == nil {
		return Closed{}, fmt.Errorf("invalid response")
	}
	return *r.Closed, nil
}

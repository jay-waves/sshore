package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// attachment owns the two connections and the terminal byte counters.
type attachment struct {
	control             attachmentControl
	terminal            net.Conn
	done                chan struct{}
	closeOnce           sync.Once
	outputBytes         uint64
	inputBytes          atomic.Uint64
	inputProgress       chan struct{}
	controlMu, outputMu sync.Mutex
	input               shellInput
	approvals           approvalExchanges
}

func newAttachment(control attachmentControl, terminal net.Conn) *attachment {
	return &attachment{control: control, terminal: terminal, done: make(chan struct{}), inputProgress: make(chan struct{}, 1)}
}
func (a *attachment) Done() <-chan struct{} { return a.done }
func (a *attachment) Close() {
	a.closeOnce.Do(func() {
		close(a.done)
		_ = a.terminal.Close()
		_ = a.control.Close()
	})
}
func (a *attachment) controlMessage(m message) error {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	return streamMessage(a.control, m)
}

// Replace the interactive launcher with an in-process request. The existing
// handoff owns confirmation, rollback and the remote spawn lifetime connection.
func serveShortcutSpawn(p *attachment, done <-chan struct{}, serve func(net.Conn)) {
	client, server := net.Pipe()
	defer client.Close()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		defer server.Close()
		serve(server)
	}()
	go func() {
		select {
		case <-done:
			client.Close()
		case <-finished:
		}
	}()
	accepted := false
	for {
		m, err := readMessage(client)
		if err != nil {
			if !accepted {
				_ = p.controlMessage(message{Type: "spawn_child_error", Error: "spawn handoff ended before confirmation"})
			}
			return
		}
		if m.Type == "handoff_accepted" {
			accepted = true
		}
		if m.Error != "" {
			_ = p.controlMessage(message{Type: "spawn_child_error", Error: m.Error})
			return
		}
	}
}

func (a *attachment) Output(data []byte) error {
	a.outputMu.Lock()
	defer a.outputMu.Unlock()
	_ = a.terminal.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := writeFull(a.terminal, data)
	_ = a.terminal.SetWriteDeadline(time.Time{})
	if err == nil {
		a.outputBytes += uint64(len(data))
	}
	return err
}

// FinishExit follows terminal output, allowing bounded consumption before close.
func (a *attachment) FinishExit(code int, reason string) {
	a.outputMu.Lock()
	_ = a.controlMessage(message{Type: "exit", Code: code, Reason: reason, TerminalOffset: a.outputBytes})
	a.outputMu.Unlock()
	select {
	case <-a.Done():
	case <-time.After(2 * time.Second):
		a.Close()
	}
}

// Detach completes the detach/confirmation/bye handshake on an exclusively owned
// control stream. Confirmation runs before bye, so callers can retain the name
// even if the final acknowledgement fails.
func detachAttachment(control attachmentControl, confirmed ...func(string)) error {
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	defer control.SetDeadline(time.Time{})
	if err := writeMessage(control, message{Type: "detach"}); err != nil {
		return err
	}
	m, err := expectDetachMessage(control, "detached")
	if err != nil {
		return err
	}
	for _, confirm := range confirmed {
		confirm(m.Name)
	}
	return writeMessage(control, message{Type: "bye"})
}

func expectDetachMessage(r io.Reader, expected string) (message, error) {
	m, err := readMessage(r)
	if err != nil {
		return m, err
	}
	if m.Type != expected {
		return m, fmt.Errorf("expected %s in detach handshake, received %s", expected, m.Type)
	}
	return m, nil
}

// FinishDetach confirms the handoff after the host releases the input lease.
// Only the control loop may call it, since it consumes the final acknowledgement.
func (a *attachment) FinishDetach(name string) error {
	defer a.Close()
	if err := a.controlMessage(message{Type: "detached", Name: name}); err != nil {
		return err
	}
	_ = a.control.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := expectDetachMessage(a.control, "bye")
	return err
}

// DrainInput acknowledges only bytes accepted by the PTY input lease. The
// counter preserves detach ordering across independent control/data channels.
func (a *attachment) DrainInput(offset uint64) error {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for a.inputBytes.Load() < offset {
		select {
		case <-a.inputProgress:
		case <-a.Done():
			return io.ErrClosedPipe
		case <-timer.C:
			// Leave the barrier unacknowledged so the client can report
			// uncertain input and send detach without losing the shell.
			return nil
		}
	}
	return a.controlMessage(message{Type: "input_drained"})
}
func (a *attachment) ReadInput(input interface {
	io.Writer
	Stopped() bool
}, active func() bool) {
	buf := make([]byte, 32*1024)
	for {
		n, err := a.terminal.Read(buf)
		if n > 0 {
			if !active() {
				return
			}
			if e := writeFull(input, buf[:n]); e != nil {
				if !input.Stopped() {
					a.Close()
				}
				return
			}
			a.inputBytes.Add(uint64(n))
			select {
			case a.inputProgress <- struct{}{}:
			default:
			}
		}
		if err != nil {
			a.Close()
			return
		}
	}
}

// The helper and attachment remain connected until the local client accepts the handoff.
func (p *attachment) requestApproval(c net.Conn, m message) error {
	token := rand.Text()
	responses, err := p.approvals.open(token)
	if err != nil {
		return err
	}
	defer p.approvals.remove(token)
	timer := time.NewTimer(requestTimeout)
	defer timer.Stop()
	m.Token = token
	if err := p.controlMessage(m); err != nil {
		return err
	}
	helperDone := make(chan error, 1)
	go func() {
		var unexpected [1]byte
		_, err := io.ReadFull(c, unexpected[:])
		helperDone <- err
	}()
	cancel := func(err error) error {
		_ = p.controlMessage(message{Type: "approval_cancelled", Token: token})
		return err
	}
	var reply message
	select {
	case reply = <-responses:
	case <-p.Done():
		return io.ErrClosedPipe
	case <-helperDone:
		return cancel(io.ErrClosedPipe)
	case <-timer.C:
		return cancel(fmt.Errorf("approval request timed out"))
	}
	if reply.Type != "approval_handoff" {
		return fileResponseError(reply)
	}
	select {
	case <-p.Done():
		return io.ErrClosedPipe
	case <-helperDone:
		return cancel(io.ErrClosedPipe)
	default:
	}
	_ = c.SetWriteDeadline(time.Now().Add(requestTimeout))
	if err := writeMessage(c, message{Type: "approval_handoff", SoftwareVersion: version}); err != nil {
		return cancel(err)
	}
	// The remote invocation owns its connection. Wait until it has received
	// the handoff and closed before the frontend detaches and switches screens.
	select {
	case err := <-helperDone:
		if err != io.EOF {
			return cancel(fmt.Errorf("approval helper did not close its request"))
		}
	case <-p.Done():
		return io.ErrClosedPipe
	case <-timer.C:
		return cancel(fmt.Errorf("approval request timed out"))
	}
	return p.controlMessage(message{Type: "approval_ack", Token: token})
}

// Only one helper can hand a request to the attached client at a time.
type approvalExchanges struct {
	mu       sync.Mutex
	token    string
	response chan message
	ready    bool
}

func (a *approvalExchanges) enable() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ready = true
}

func (a *approvalExchanges) disable() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ready = false
}

func (a *approvalExchanges) open(token string) (<-chan message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ready {
		return nil, fmt.Errorf("attached client unavailable for approval")
	}
	if a.response != nil {
		return nil, fmt.Errorf("another approval is in progress")
	}
	a.token = token
	a.response = make(chan message, 1)
	return a.response, nil
}
func (a *approvalExchanges) remove(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == token {
		a.token, a.response = "", nil
	}
}
func (a *approvalExchanges) reply(m message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != m.Token {
		return
	}
	select {
	case a.response <- m:
	default:
	}
}

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
	timer := time.AfterFunc(5*time.Second, func() {
		r.mu.Lock()
		pending := r.pending[token] == endpoint
		if pending {
			delete(r.pending, token)
		}
		r.mu.Unlock()
		if pending {
			endpoint.Close()
		}
	})
	defer timer.Stop()
	defer func() { r.mu.Lock(); delete(r.pending, token); r.mu.Unlock() }()
	if err := writeMessage(c, message{Type: "terminal_ready", SoftwareVersion: version, Token: token}); err != nil {
		return err
	}
	<-endpoint.done
	return nil
}
func (r *terminalEndpoints) take(token string) (net.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.pending[token]
	if c == nil {
		return nil, fmt.Errorf("terminal endpoint expired or already claimed")
	}
	delete(r.pending, token)
	return c, nil
}

func openTerminalEndpoint(ctx context.Context, o options) (stream net.Conn, token string, err error) {
	if o.reusable != nil {
		stream, err = openReusableTransport(ctx, o.reusable)
	} else {
		o.command = "attach"
		stream, err = openClientTransportContext(ctx, &o, nil)
	}
	if err != nil {
		return nil, "", err
	}
	timer := time.AfterFunc(5*time.Second, func() { stream.Close() })
	defer timer.Stop()
	defer func() {
		if err != nil {
			stream.Close()
		}
	}()
	reply, err := exchangeRouteRequest(stream, message{Type: "terminal_open"})
	if err != nil {
		return stream, "", err
	}
	if err = checkPeerVersion(reply.SoftwareVersion, "terminal endpoint"); err != nil {
		return stream, "", err
	}
	if reply.Type != "terminal_ready" || len(reply.Token) < 26 || len(reply.Token) > 128 {
		return stream, "", fmt.Errorf("invalid terminal endpoint")
	}
	return stream, reply.Token, nil
}

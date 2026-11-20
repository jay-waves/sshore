package ssh

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

// Client owns one authenticated connection and its jump chain.
// Closing a session never transfers connection ownership to another task.
type Client struct {
	*cryptossh.Client
	jump *Client
	once sync.Once
	done chan struct{}
	keys []cryptossh.Signer
}

func (c *Client) Close() error {
	c.once.Do(func() {
		close(c.done)
		// A tunneled connection can block sending channel-close on its jump.
		// Close the owned chain from the TCP connection outward.
		if c.jump != nil {
			c.jump.Close()
		}
		c.Client.Close()
		clear(c.keys)
		c.keys = nil
	})
	return nil
}

func Dial(ctx context.Context, o Options) (*Client, error) {
	var budgets authenticationBudgets
	return dial(ctx, o, 0, &budgets)
}
func dial(ctx context.Context, o Options, depth int, budgets *authenticationBudgets) (*Client, error) {
	if depth > 8 {
		return nil, fmt.Errorf("SSH ProxyJump chain exceeds eight hops")
	}
	cfg, err := resolveDialConfig(ctx, o)
	if err != nil {
		return nil, err
	}
	return cfg.dial(ctx, depth, budgets)
}

func resolveDialConfig(ctx context.Context, o Options) (*connectionConfig, error) {
	configContext, stopConfig := context.WithTimeout(ctx, 15*time.Second)
	defer stopConfig()
	return resolveConfig(configContext, o)
}

func (cfg *connectionConfig) dial(ctx context.Context, depth int, budgets *authenticationBudgets) (*Client, error) {
	client, err := cfg.handshake(ctx, depth, budgets)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.ToUpper(cfg.get("loglevel")), "DEBUG") {
		o := cfg.options
		if metadata, ok := client.Conn.(cryptossh.AlgorithmsConnMetadata); ok {
			out := o.Diagnostic
			if out == nil {
				out = os.Stderr
			}
			algorithms := metadata.Algorithms()
			direction := func(a cryptossh.DirectionAlgorithms) string {
				if a.MAC == "" {
					return a.Cipher + " (AEAD)"
				}
				return a.Cipher + " / " + a.MAC
			}
			fmt.Fprintf(out, "[sshore] SSH %q: kex=%s hostkey=%s send=%s receive=%s\n", o.Host, algorithms.KeyExchange, algorithms.HostKey, direction(algorithms.Write), direction(algorithms.Read))
		}
	}
	client.keepalive(cfg.get("serveraliveinterval"), cfg.get("serveralivecountmax"))
	return client, nil
}

func (cfg *connectionConfig) handshake(ctx context.Context, depth int, budgets *authenticationBudgets) (client *Client, resultErr error) {
	connecting, cancel := context.WithCancel(ctx)
	defer cancel()
	releaseBudget := budgets.add(cfg.timeout, cancel)
	cc := &cryptossh.ClientConfig{User: cfg.user}
	var keys []cryptossh.Signer
	if err := cfg.configureAuthentication(connecting, cc, budgets, &keys); err != nil {
		releaseBudget()
		return nil, err
	}
	var parent *Client
	var stopTransport func() bool
	defer func() {
		releaseBudget()
		if stopTransport != nil {
			stopTransport()
		}
		// Disarm cancellation before checking whether the client can be
		// handed off. A cancellation that already fired still wins.
		if client != nil {
			if err := connecting.Err(); err != nil {
				client.Close()
				client, resultErr = nil, err
			}
		} else if parent != nil {
			parent.Close()
		}
	}()
	modern := cryptossh.SupportedAlgorithms()
	var err error
	choose := func(key string, allowed []string) ([]string, error) {
		value := cfg.get(key)
		if value == "" {
			return allowed, nil
		}
		var result []string
		for a := range strings.SplitSeq(value, ",") {
			if slices.Contains(allowed, a) {
				result = append(result, a)
			}
		}
		if len(result) == 0 {
			return nil, fmt.Errorf("%s has no supported modern algorithm", key)
		}
		return result, nil
	}
	if cc.KeyExchanges, err = choose("kexalgorithms", modern.KeyExchanges); err != nil {
		return nil, err
	}
	if cc.Ciphers, err = choose("ciphers", modern.Ciphers); err != nil {
		return nil, err
	}
	if cc.MACs, err = choose("macs", modern.MACs); err != nil {
		return nil, err
	}
	address := net.JoinHostPort(cfg.host, cfg.port)
	var raw net.Conn
	if jump := cfg.get("proxyjump"); jump != "" && jump != "none" {
		hop, jumpErr := dial(connecting, cfg.proxyJumpOptions(jump), depth+1, budgets)
		if jumpErr != nil {
			return nil, jumpErr
		}
		// Closing the jump unblocks both a pending channel open and I/O
		// on the channel once it exists.
		parent = hop
		stopTransport = context.AfterFunc(connecting, func() { _ = hop.Close() })
		raw, err = hop.Dial("tcp", address)
	} else {
		dialer := net.Dialer{}
		if bind := cfg.get("bindaddress"); bind != "" && bind != "*" {
			dialer.LocalAddr, err = net.ResolveTCPAddr(cfg.network, net.JoinHostPort(bind, "0"))
			if err != nil {
				return nil, err
			}
		}
		raw, err = dialer.DialContext(connecting, cfg.network, address)
		if err == nil {
			stopTransport = context.AfterFunc(connecting, func() { _ = raw.Close() })
		}
	}
	if err != nil {
		return nil, err
	}
	// NewClientConn takes ownership of raw, including on handshake failure.
	conn, ch, req, err := cryptossh.NewClientConn(raw, address, cc)
	if err != nil {
		return nil, fmt.Errorf("SSH %s: %w", cfg.options.Host, err)
	}
	return &Client{Client: cryptossh.NewClient(conn, ch, req), jump: parent, done: make(chan struct{}), keys: keys}, nil
}
func (cfg *connectionConfig) proxyJumpOptions(jump string) Options {
	last := strings.LastIndexByte(jump, ',')
	host := jump[last+1:]
	o := cfg.options
	options := Options{
		Host: host, Executable: o.Executable, Environment: o.Environment,
		Directory: o.Directory, Confirm: o.Confirm, Secret: o.Secret, Diagnostic: o.Diagnostic,
	}
	for args := o.Args; len(args) > 0; {
		name, _, n := SplitOption(args)
		if name == "-F" && n > 0 {
			options.Args = append(options.Args, args[:n]...)
		}
		args = args[max(n, 1):]
	}
	if last >= 0 {
		options.Args = append(options.Args, "-J", jump[:last])
	}
	// ssh -G accepts user@host but not ProxyJump's optional :port suffix.
	if at := strings.LastIndex(host, ":"); at >= 0 && at > strings.LastIndex(host, "]") {
		if _, err := strconv.Atoi(host[at+1:]); err == nil {
			options.Args = append(options.Args, "-p", host[at+1:])
			options.Host = host[:at]
		}
	}
	return options
}

func (c *Client) keepalive(interval, count string) {
	seconds, _ := strconv.Atoi(interval)
	if seconds <= 0 {
		seconds = 15
	}
	limit, _ := strconv.Atoi(count)
	if limit <= 0 {
		limit = 3
	}
	go func() {
		timer := time.NewTicker(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		deadline := time.Duration(seconds*limit) * time.Second
		for {
			select {
			case <-c.done:
				return
			case <-timer.C:
				timeout := time.AfterFunc(deadline, func() { c.Close() })
				_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
				if !timeout.Stop() || err != nil {
					c.Close()
					return
				}
			}
		}
	}()
}

// Process is an SSH exec channel. CloseWrite sends channel EOF; Close aborts
// its I/O and closes the connection only when created by the package-level Start.
// Wait must be called after consuming stdout.
type Process struct {
	stdin  processInput
	stdout io.Reader
	client *Client
	wait   func() error
	close  func() error
}

// DialTCP opens a sibling TCP channel. Closing it leaves the exec channel alive.
func (p *Process) DialTCP(ctx context.Context, address string) (net.Conn, error) {
	return p.client.DialContext(ctx, "tcp", address)
}

func (p *Process) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *Process) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// CloseWrite sends EOF while the caller continues reading stdout.
func (p *Process) CloseWrite() error { return p.stdin.Close() }
func (p *Process) Close() error {
	return p.close()
}

// Wait returns the same exit result to every caller, including concurrent ones.
func (p *Process) Wait() error { return p.wait() }

// CloseAndWait aborts I/O before joining the exec channel. Normal completion
// should consume stdout and call Wait first to preserve all remote output.
func (p *Process) CloseAndWait() error {
	_ = p.Close()
	return p.Wait()
}

// OpenSibling starts another exec channel on this process's SSH connection.
func (p *Process) OpenSibling(command string, diagnostic io.Writer) (*Process, error) {
	return p.client.Start(command, diagnostic)
}

func Start(ctx context.Context, o Options, diagnostic io.Writer) (process *Process, err error) {
	if o.Diagnostic == nil {
		o.Diagnostic = diagnostic
	}
	client, err := Dial(ctx, o)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer func() {
		if process == nil {
			stop()
			client.Close()
		}
	}()
	return startSession(client, o.Command, diagnostic, stop)
}

// Start opens an exec channel owned by the caller's connection.
func (c *Client) Start(command string, diagnostic io.Writer) (*Process, error) {
	return startSession(c, command, diagnostic, nil)
}

func startSession(client *Client, command string, diagnostic io.Writer, stop func() bool) (*Process, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	p := &Process{client: client, wait: sync.OnceValue(session.Wait), close: session.Close}
	if stop != nil {
		p.close = func() error {
			stop()
			return client.Close()
		}
	}
	session.Stderr = diagnostic
	p.stdin.writer, err = session.StdinPipe()
	if err == nil {
		p.stdout, err = session.StdoutPipe()
	}
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	if err = session.Start(command); err != nil {
		_ = session.Close()
		return nil, err
	}
	return p, nil
}
func Run(ctx context.Context, o Options, input io.Reader, output, diagnostic io.Writer) error {
	p, err := Start(ctx, o, diagnostic)
	if err != nil {
		return err
	}
	defer p.CloseAndWait()
	// Input may be an interactive terminal. Application owns and cancels its reader.
	if input != nil {
		go func() { io.Copy(p, input); p.CloseWrite() }()
	} else {
		p.CloseWrite()
	}
	if output == nil {
		output = io.Discard
	}
	_, err = io.Copy(output, p)
	if err != nil {
		return err
	}
	return p.Wait()
}

// crypto/ssh's session stdin does not serialize CloseWrite with concurrent writes.
type processInput struct {
	mu     sync.Mutex
	writer io.WriteCloser
}

func (p *processInput) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writer == nil {
		return 0, io.ErrClosedPipe
	}
	return p.writer.Write(data)
}
func (p *processInput) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writer == nil {
		return nil
	}
	writer := p.writer
	p.writer = nil
	return writer.Close()
}

// A prompt suspends connection budgets across the jump chain. Each hop
// retains its own deadline, but only the earliest deadline needs a timer.
type authenticationBudget struct {
	deadline time.Time
	cancel   context.CancelFunc
}

type authenticationBudgets struct {
	mu       sync.Mutex
	entries  []*authenticationBudget
	timer    *time.Timer
	paused   int
	pausedAt time.Time
}

func (g *authenticationBudgets) scheduleLocked() {
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	now := time.Now()
	if g.paused != 0 {
		now = g.pausedAt
	}
	var earliest time.Time
	g.entries = slices.DeleteFunc(g.entries, func(entry *authenticationBudget) bool {
		if !entry.deadline.After(now) {
			entry.cancel()
			return true
		}
		if earliest.IsZero() || entry.deadline.Before(earliest) {
			earliest = entry.deadline
		}
		return false
	})
	if earliest.IsZero() || g.paused != 0 {
		return
	}

	var timer *time.Timer
	timer = time.AfterFunc(time.Until(earliest), func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.timer == timer {
			g.timer = nil
			g.scheduleLocked()
		}
	})
	g.timer = timer
}

func (g *authenticationBudgets) add(duration time.Duration, cancel context.CancelFunc) func() {
	g.mu.Lock()
	start := time.Now()
	if g.paused != 0 {
		start = g.pausedAt
	}
	entry := &authenticationBudget{deadline: start.Add(duration), cancel: cancel}
	g.entries = append(g.entries, entry)
	g.scheduleLocked()
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if i := slices.Index(g.entries, entry); i >= 0 {
			g.entries = slices.Delete(g.entries, i, i+1)
			g.scheduleLocked()
		}
	}
}

func (g *authenticationBudgets) pause() func() {
	g.mu.Lock()
	g.paused++
	if g.paused == 1 {
		g.pausedAt = time.Now()
		g.scheduleLocked()
	}
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.paused--
		if g.paused == 0 {
			waited := time.Since(g.pausedAt)
			for _, entry := range g.entries {
				entry.deadline = entry.deadline.Add(waited)
			}
			g.scheduleLocked()
		}
	}
}

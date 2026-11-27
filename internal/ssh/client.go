package ssh

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
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
	jump        *Client
	once        sync.Once
	done        chan struct{}
	destination Destination
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
	cfg.warnIgnoredForwarding(ctx)
	for _, key := range []string{"hostname", "user", "port"} {
		if source := cfg.origins[key]; source != "" {
			cfg.debugf(ctx, "SSH config %s from %q", key, source)
		}
	}
	client, err := cfg.handshake(ctx, depth, budgets)
	if err != nil {
		return nil, err
	}
	interval, _ := strconv.Atoi(cfg.get("serveraliveinterval"))
	count, _ := strconv.Atoi(cfg.get("serveralivecountmax"))
	client.keepalive(time.Duration(interval)*time.Second, count)
	return client, nil
}

func (cfg *connectionConfig) warnIgnoredForwarding(ctx context.Context) {
	if cfg.get("localforward") == "" && cfg.get("remoteforward") == "" && cfg.get("dynamicforward") == "" {
		return
	}
	if report := cfg.options.IgnoredForwarding; report != nil {
		report(ctx, cfg.options.Host)
	}
}

func (cfg *connectionConfig) handshake(ctx context.Context, depth int, budgets *authenticationBudgets) (client *Client, resultErr error) {
	connecting, cancel := context.WithCancel(ctx)
	defer cancel()
	seconds, _ := strconv.Atoi(cfg.get("connecttimeout"))
	releaseBudget := budgets.add(time.Duration(seconds)*time.Second, cancel)
	cc := &cryptossh.ClientConfig{User: cfg.user}
	authenticatedWith := "none"
	if err := cfg.configureAuthentication(connecting, cc, budgets, &authenticatedWith); err != nil {
		releaseBudget()
		return nil, err
	}
	var reportNegotiation func(cryptossh.ConnMetadata, cryptossh.NegotiatedAlgorithms)
	if cfg.debugEnabled() {
		reported := false
		reportNegotiation = func(metadata cryptossh.ConnMetadata, a cryptossh.NegotiatedAlgorithms) {
			if reported {
				return
			}
			reported = true
			cfg.debugf(connecting, "Server version %q", metadata.ServerVersion())
			cfg.debugf(connecting, "Negotiated kex=%s hostkey=%s send=%s receive=%s",
				a.KeyExchange, a.HostKey, describeDirection(a.Write), describeDirection(a.Read))
		}
		auth := cc.AuthCallback
		cc.AuthCallback = func(state *cryptossh.ClientAuthContext) (cryptossh.AuthMethod, error) {
			reportNegotiation(state.Metadata, state.Algorithms)
			return auth(state)
		}
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
	var err error
	cc.KeyExchanges = cfg.values["kexalgorithms"]
	cc.Ciphers = cfg.values["ciphers"]
	cc.MACs = cfg.values["macs"]
	address := net.JoinHostPort(cfg.host, cfg.port)
	var raw net.Conn
	jump := cfg.get("proxyjump")
	if jump != "" && jump != "none" {
		cfg.debugf(connecting, "Connecting to %s as %q via ProxyJump %q", address, cfg.user, jump)
		options, jumpErr := cfg.proxyJumpOptions(jump)
		if jumpErr != nil {
			return nil, jumpErr
		}
		hop, jumpErr := dial(connecting, options, depth+1, budgets)
		if jumpErr != nil {
			return nil, jumpErr
		}
		// Closing the jump unblocks both a pending channel open and I/O
		// on the channel once it exists.
		parent = hop
		stopTransport = context.AfterFunc(connecting, func() { _ = hop.Close() })
		raw, err = hop.Dial("tcp", address)
	} else {
		cfg.debugf(connecting, "Connecting to %s as %q", address, cfg.user)
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
	cfg.debugf(connecting, "Connection established to %s", raw.RemoteAddr())
	// NewClientConn takes ownership of raw, including on handshake failure.
	conn, ch, req, err := cryptossh.NewClientConn(raw, address, cc)
	if err != nil {
		return nil, fmt.Errorf("SSH %s: %w", cfg.options.Host, err)
	}
	if reportNegotiation != nil {
		if metadata, ok := conn.(cryptossh.AlgorithmsConnMetadata); ok {
			reportNegotiation(conn, metadata.Algorithms())
		}
		cfg.debugf(connecting, "Authenticated using %s", authenticatedWith)
	}
	peerIP := ""
	if parent == nil {
		if addr, ok := raw.RemoteAddr().(*net.TCPAddr); ok {
			peerIP = addr.IP.String()
			if addr.Zone != "" {
				peerIP += "%" + addr.Zone
			}
		}
	}
	return &Client{Client: cryptossh.NewClient(conn, ch, req), jump: parent, done: make(chan struct{}), destination: Destination{User: cfg.user, Host: cfg.host, Port: cfg.port, PeerIP: peerIP, Jump: parent != nil}}, nil
}
func describeDirection(a cryptossh.DirectionAlgorithms) string {
	if a.MAC == "" {
		return a.Cipher + " (AEAD)"
	}
	return a.Cipher + " / " + a.MAC
}

func (cfg *connectionConfig) proxyJumpOptions(jump string) (Options, error) {
	last := strings.LastIndexByte(jump, ',')
	host := jump[last+1:]
	o := cfg.options
	options := Options{
		Host: host, Environment: o.Environment,
		Directory: o.Directory, Confirm: o.Confirm, Secret: o.Secret,
		IgnoredForwarding: o.IgnoredForwarding, Diagnostic: o.Diagnostic,
		RequireKnownHost: o.RequireKnownHost,
	}
	for args := o.Args; len(args) > 0; {
		name, _, n := SplitOption(args)
		if name == "-F" && n > 0 {
			options.Args = append(options.Args, args[:n]...)
		}
		args = args[max(n, 1):]
	}
	if cfg.debugEnabled() {
		options.Args = append(options.Args, "-v")
	}
	if last >= 0 {
		options.Args = append(options.Args, "-J", jump[:last])
	}
	// An unbracketed IP literal carries no port.
	prefix, address := "", host
	if user, tail, ok := strings.Cut(host, "@"); ok {
		prefix, address = user+"@", tail
	}
	literal := address
	if strings.HasPrefix(literal, "[") && strings.HasSuffix(literal, "]") {
		literal = literal[1 : len(literal)-1]
	}
	if _, err := netip.ParseAddr(literal); err == nil {
		return options, nil
	}
	if strings.Contains(address, ":") {
		name, port, err := net.SplitHostPort(address)
		if err != nil || name == "" || port == "" {
			return Options{}, fmt.Errorf("invalid SSH ProxyJump destination %q", host)
		}
		options.Args = append(options.Args, "-p", port)
		options.Host = prefix + name
	}
	return options, nil
}

func (c *Client) keepalive(interval time.Duration, count int) {
	if interval == 0 {
		return
	}
	if count == 0 {
		count = 3
	}
	deadline := min(time.Duration(count), math.MaxInt64/interval) * interval
	go func() {
		timer := time.NewTicker(interval)
		defer timer.Stop()
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
	command string
	stdin   processInput
	stdout  io.Reader
	client  *Client
	wait    func() error
	close   func() error
}

type Destination struct {
	User, Host, Port, PeerIP string
	Jump                     bool
}

func (d Destination) ObservedAddress(serverIP string) string {
	if !d.Jump && d.PeerIP != "" {
		return d.PeerIP
	}
	if d.Jump {
		// The jump socket does not expose the final peer. Prefer the address
		// the destination sshd reports for its own side of this connection.
		if addr, err := netip.ParseAddr(serverIP); err == nil {
			return addr.Unmap().String()
		}
	}
	return d.Host
}

func (c *Client) Destination() Destination { return c.destination }

func (p *Process) Destination() Destination { return p.client.Destination() }

// DialTCP opens a sibling TCP channel. Closing it leaves the exec channel alive.
func (p *Process) DialTCP(ctx context.Context, address string) (net.Conn, error) {
	return p.client.DialTCP(ctx, address)
}

func (c *Client) DialTCP(ctx context.Context, address string) (net.Conn, error) {
	return c.DialContext(ctx, "tcp", address)
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
func (p *Process) OpenSibling(ctx context.Context, o Options, diagnostic io.Writer) (*Process, error) {
	if o.OpenCommand != nil {
		return o.OpenCommand(ctx, p.client, p.command, diagnostic)
	}
	return p.client.Start(o.Command, diagnostic)
}

func Start(ctx context.Context, o Options, diagnostic io.Writer) (process *Process, err error) {
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
	if o.OpenCommand != nil {
		process, err = o.OpenCommand(ctx, client, "", diagnostic)
	} else {
		process, err = client.Start(o.Command, diagnostic)
	}
	if err != nil {
		return nil, err
	}
	process.close = func() error {
		stop()
		return client.Close()
	}
	return process, nil
}

// Start opens an exec channel owned by the caller's connection.
func (c *Client) Start(command string, diagnostic io.Writer) (*Process, error) {
	session, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	p := &Process{client: c, command: command, wait: sync.OnceValue(session.Wait), close: session.Close}
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
	if duration == 0 {
		return func() {}
	}
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

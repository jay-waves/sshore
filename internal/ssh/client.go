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

// Client owns one authenticated connection, its jump chain and agent connections.
// Closing a session never transfers connection ownership to another task.
type Client struct {
	*cryptossh.Client
	config    *connectionConfig
	resources []io.Closer
	once      sync.Once
	done      chan struct{}
	forwards  forwarding
}

func (c *Client) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.Client.Close()
		c.forwards.close()
		for i := len(c.resources) - 1; i >= 0; i-- {
			c.resources[i].Close()
		}
		c.forwards.workers.Wait()
	})
	return nil
}

func Dial(ctx context.Context, o Options) (*Client, error) { return dial(ctx, o, 0) }
func dial(ctx context.Context, o Options, depth int) (result *Client, err error) {
	if depth > 8 {
		return nil, fmt.Errorf("SSH ProxyJump chain exceeds eight hops")
	}
	configContext, stopConfig := context.WithTimeout(ctx, 15*time.Second)
	cfg, err := resolveConfig(configContext, o)
	stopConfig()
	if err != nil {
		return nil, err
	}
	connecting, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	callback, hostAlgos, err := cfg.hostKeys(connecting)
	if err != nil {
		return nil, err
	}
	auth, agentConn, err := cfg.authentication(connecting)
	if err != nil {
		return nil, err
	}
	stopAgent := context.AfterFunc(connecting, func() { agentConn.Close() })
	defer stopAgent()
	resources := []io.Closer{agentConn}
	defer func() {
		if err != nil {
			for i := len(resources) - 1; i >= 0; i-- {
				resources[i].Close()
			}
		}
	}()
	cc := &cryptossh.ClientConfig{User: cfg.user, Auth: auth, HostKeyCallback: callback, HostKeyAlgorithms: hostAlgos}
	modern := cryptossh.SupportedAlgorithms()
	choose := func(key string, allowed []string) ([]string, error) {
		value := cfg.get(key)
		if value == "" {
			return allowed, nil
		}
		var result []string
		for _, a := range strings.Split(value, ",") {
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
		hops := strings.Split(jump, ",")
		first := hops[len(hops)-1]
		jo := Options{Host: first, Executable: cfg.options.Executable, Environment: cfg.options.Environment, Directory: o.Directory, Confirm: o.Confirm, Diagnostic: o.Diagnostic}
		for i := 0; i < len(o.Args); i++ {
			if o.Args[i] == "-F" && i+1 < len(o.Args) {
				jo.Args = append(jo.Args, "-F", o.Args[i+1])
				i++
			} else if strings.HasPrefix(o.Args[i], "-F") && len(o.Args[i]) > 2 {
				jo.Args = append(jo.Args, o.Args[i])
			}
		}
		if len(hops) > 1 {
			jo.Args = append(jo.Args, "-J", strings.Join(hops[:len(hops)-1], ","))
		}
		// ssh -G accepts user@host but not ProxyJump's optional :port suffix.
		if at := strings.LastIndex(first, ":"); at >= 0 && (!strings.Contains(first, "]") || at > strings.LastIndex(first, "]")) {
			if _, e := strconv.Atoi(first[at+1:]); e == nil {
				jo.Args = append(jo.Args, "-p", first[at+1:])
				jo.Host = first[:at]
			}
		}
		parent, e := dial(connecting, jo, depth+1)
		if e != nil {
			return nil, e
		}
		resources = append(resources, parent)
		stop := context.AfterFunc(connecting, func() { parent.Close() })
		raw, err = parent.Dial("tcp", address)
		stop()
	} else {
		dialer := net.Dialer{}
		if bind := cfg.get("bindaddress"); bind != "" && bind != "*" {
			dialer.LocalAddr, err = net.ResolveTCPAddr(cfg.network, net.JoinHostPort(bind, "0"))
			if err != nil {
				return nil, err
			}
		}
		raw, err = dialer.DialContext(connecting, cfg.network, address)
	}
	if err != nil {
		return nil, err
	}
	resources = append(resources, raw)
	stop := context.AfterFunc(connecting, func() { raw.Close() })
	defer stop()
	if deadline, ok := connecting.Deadline(); ok {
		raw.SetDeadline(deadline)
	}
	conn, ch, req, err := cryptossh.NewClientConn(raw, address, cc)
	if err != nil {
		return nil, fmt.Errorf("SSH %s: %w", o.Host, err)
	}
	if err = connecting.Err(); err != nil {
		conn.Close()
		return nil, err
	}
	raw.SetDeadline(time.Time{})
	if strings.HasPrefix(strings.ToUpper(cfg.get("loglevel")), "DEBUG") {
		if metadata, ok := conn.(cryptossh.AlgorithmsConnMetadata); ok {
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
	client := &Client{Client: cryptossh.NewClient(conn, ch, req), config: cfg, resources: resources, done: make(chan struct{})}
	client.keepalive()
	return client, nil
}
func (c *Client) keepalive() {
	seconds, _ := strconv.Atoi(c.config.get("serveraliveinterval"))
	if seconds <= 0 {
		seconds = 15
	}
	limit, _ := strconv.Atoi(c.config.get("serveralivecountmax"))
	if limit <= 0 {
		limit = 3
	}
	go func() {
		timer := time.NewTicker(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-timer.C:
				result := make(chan error, 1)
				go func() { _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); result <- err }()
				timeout := time.NewTimer(time.Duration(seconds*limit) * time.Second)
				select {
				case err := <-result:
					timeout.Stop()
					if err != nil {
						c.Close()
						return
					}
				case <-timeout.C:
					c.Close()
					return
				case <-c.done:
					timeout.Stop()
					return
				}
			}
		}
	}()
}

// Process is an SSH exec channel and its owning connection. Input.Close sends
// channel EOF; Close aborts all I/O. Wait must be called after consuming Output.
type Process struct {
	Input   io.WriteCloser
	Output  io.ReadCloser
	session *cryptossh.Session
	client  *Client
	stop    func() bool
	once    sync.Once
}

// DialSocket opens a daemon channel on the already authenticated connection.
func (p *Process) DialSocket(ctx context.Context, path string) (net.Conn, error) {
	return p.client.DialContext(ctx, "unix", path)
}

func (p *Process) Read(b []byte) (int, error)  { return p.Output.Read(b) }
func (p *Process) Write(b []byte) (int, error) { return p.Input.Write(b) }
func (p *Process) Close() error {
	p.once.Do(func() {
		if p.stop != nil {
			p.stop()
		}
		p.client.Close()
		p.session.Close()
	})
	return nil
}
func (p *Process) Wait() error { return p.session.Wait() }
func Start(ctx context.Context, o Options, diagnostic io.Writer) (*Process, error) {
	if o.Diagnostic == nil {
		o.Diagnostic = diagnostic
	}
	client, err := Dial(ctx, o)
	if err != nil {
		return nil, err
	}
	stopConnecting := context.AfterFunc(ctx, func() { client.Close() })
	defer stopConnecting()
	if err := client.StartForwarding(); err != nil {
		client.Close()
		return nil, err
	}
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, err
	}
	p := &Process{session: session, client: client}
	session.Stderr = diagnostic
	var input io.WriteCloser
	input, err = session.StdinPipe()
	p.Input = &processInput{writer: input}
	if err == nil {
		var output io.Reader
		output, err = session.StdoutPipe()
		p.Output = processOutput{Reader: output, close: p.Close}
	}
	if err != nil {
		p.Close()
		return nil, err
	}
	p.stop = context.AfterFunc(ctx, func() { client.Close() })
	if err = session.Start(o.Command); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}
func Run(ctx context.Context, o Options, input io.Reader, output, diagnostic io.Writer) error {
	p, err := Start(ctx, o, diagnostic)
	if err != nil {
		return err
	}
	defer p.Close()
	// Input may be an interactive terminal. Application owns and cancels its reader.
	if input != nil {
		go func() { io.Copy(p.Input, input); p.Input.Close() }()
	} else {
		p.Input.Close()
	}
	if output == nil {
		output = io.Discard
	}
	_, err = io.Copy(output, p.Output)
	if err != nil {
		return err
	}
	return p.Wait()
}

type processOutput struct {
	io.Reader
	close func() error
}

func (p processOutput) Close() error { return p.close() }

// crypto/ssh's session stdin does not serialize CloseWrite with concurrent writes.
type processInput struct {
	mu     sync.Mutex
	writer io.WriteCloser
	closed bool
}

func (p *processInput) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	return p.writer.Write(data)
}
func (p *processInput) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.writer.Close()
}

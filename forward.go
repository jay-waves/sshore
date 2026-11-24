package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	forwardio "github.com/jay-waves/sshore/internal/forward"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

func parsePort(s string) (int, error) {
	n, err := parseForwardPort(s)
	if err != nil {
		return 0, usage("%s", err)
	}
	return n, nil
}

func splitPortEndpoint(value string) (string, string, error) {
	if value != "" && !strings.Contains(value, ":") {
		return "here", value, nil
	}
	return splitEndpoint(value)
}

// A qualified endpoint identifies the peer; the first endpoint always listens.
func parseForwardEndpoints(args []string) (string, int, int, bool, error) {
	fail := func() (string, int, int, bool, error) {
		return "", 0, 0, false, usage("invalid port mapping; use forward here:[PORT] HOST:PORT or forward HOST:PORT here:[PORT]")
	}
	if len(args) != 2 {
		return fail()
	}
	endpoint := func(value string) (string, int, error) {
		host, port, err := splitPortEndpoint(value)
		if err != nil {
			return "", 0, err
		}
		if host == "origin" {
			return "", 0, usage("origin: requires an attached sshore shell")
		}
		if host == "here" {
			if port == "" {
				return "", 0, nil
			}
			host = ""
		}
		n, err := parsePort(port)
		return host, n, err
	}
	left, port, err := endpoint(args[0])
	if err != nil {
		return "", 0, 0, false, err
	}
	right, target, err := endpoint(args[1])
	if err != nil {
		return "", 0, 0, false, err
	}
	if (left == "") == (right == "") {
		return fail()
	}
	host := right
	if left != "" {
		host = left
	}
	return host, port, target, left != "", nil
}

func parseForwardCommand(args []string) (forwardConfig, bool, error) {
	var f forwardConfig
	parsed, err := parseCLI(args, "forward")
	if err != nil {
		return f, false, err
	}
	f.SSH = parsed.ssh
	f.Retries = parsed.integer("--retries")
	foreground := parsed.has("--foreground")
	if foreground && f.Retries != 0 {
		return f, false, usage("--retries applies only to background forwards")
	}
	f.Host, f.Port, f.Target, f.Reverse, err = parseForwardEndpoints(parsed.operands)
	return f, foreground, err
}

func remoteForwardRequest(args []string) (operation, error) {
	op := operation{Kind: "forward"}
	if len(args) != 2 {
		return op, usage("forward requires a listener and a service")
	}
	leftHost, leftPort, err := splitPortEndpoint(args[0])
	if err != nil {
		return op, err
	}
	rightHost, rightPort, err := splitPortEndpoint(args[1])
	if err != nil {
		return op, err
	}
	var remote string
	switch {
	case leftHost == "origin" && rightHost == "here":
		op.LocalHint, remote = leftPort, rightPort
	case leftHost == "here" && rightHost == "origin":
		remote, op.LocalHint, op.Reverse = leftPort, rightPort, true
	default:
		return op, usage("forward between here:PORT and origin:[PORT]")
	}
	if op.LocalHint != "" {
		if _, err := parsePort(op.LocalHint); err != nil {
			return op, err
		}
	}
	port, err := parsePort(remote)
	if op.Reverse {
		op.Port = port
	} else {
		op.TargetPort = port
	}
	return op, err
}

// Independent SSH workers own forwarding; no listener belongs to a shell attachment.

func (f forwardConfig) direction() string {
	if f.Reverse {
		return "<-"
	}
	return "->"
}

func (f forwardConfig) description(local, remote string) string {
	localPort, remotePort := f.Port, f.Target
	if f.Reverse {
		localPort, remotePort = remotePort, localPort
	}
	return sshio.FormatEndpoint(local, strconv.Itoa(localPort)) + " " + f.direction() + " " +
		sshio.FormatEndpoint(remote, strconv.Itoa(remotePort))
}

func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// Local requests default to independent background workers; --foreground is
// available for diagnostics. Remote shell
// requests inherit the session and require local approval. Port commands resolve
// server/local ports into fixed listener and target identities. Native SSH
// configuration/authentication remains effective.

// Remote helper selection is application policy; the engine receives opaque command text.
func forwardRemoteCommand(f forwardConfig) string {
	req := remoteServiceRequest{Operation: "task-watch"}
	if f.ApprovalID != "" {
		req = remoteServiceRequest{Operation: "forward-watch", Name: f.Session, Instance: f.Instance, PID: f.ResumePID, ApprovalID: f.ApprovalID}
	}
	return remoteHelperCommand(f.Host, f.Server, req)
}

func runForwardForeground(f forwardConfig) (int, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	termui.Log(os.Stderr, "", termui.Info, "Starting %s; Ctrl-C stops it.", cleanText(f.description("local", f.Host)))
	return runForwardTransportForeground(ctx, f, os.Stderr)
}

func pendingPortDescription(op operation) string {
	if op.Reverse {
		return fmt.Sprintf("Remote listener %s -> local service", loopback(op.ServerPort()))
	}
	return fmt.Sprintf("Local listener -> remote service %s", loopback(op.ServerPort()))
}

func resolveLocalPort(op *operation, port int) {
	if op.Reverse {
		op.TargetPort = port
	} else {
		op.Port = port
	}
	op.LocalHint = ""
}

// Keep persisted task identity together with its forwarding configuration.
type forwardConfig struct {
	Instance            string `json:",omitempty"`
	Connection          *recentConnection
	Retries             int
	Session, ApprovalID string
	ResumePID           int
	Host, Name, Server  string
	Port, Target        int
	Reverse             bool
	SSH                 []string
}

func validateForwardConfig(f forwardConfig) error {
	if f.Connection != nil {
		if err := validateRecentConnection(*f.Connection); err != nil {
			return err
		}
		if !f.Connection.viaShell() || f.Connection.Session != f.Session || f.Connection.Host != f.Host || f.ApprovalID == "" {
			return fmt.Errorf("invalid routed forward approval")
		}
	}
	transportErr := f.Validate()
	if errors.Is(transportErr, errForwardRetries) {
		return usage("--retries must be between 0 and 10")
	}
	if e := validSession(f.Name); f.Name != "" && e != nil {
		return e
	}
	if e := validSSHDestination(f.Host); e != nil {
		return e
	}
	if f.ApprovalID != "" || f.Session != "" || f.ResumePID != 0 {
		if !validRequestID(f.ApprovalID) || validSession(f.Session) != nil || f.ResumePID <= 0 || f.Retries != 0 {
			return fmt.Errorf("invalid approved forward identity")
		}
	}
	return transportErr
}
func startForwardSession(f forwardConfig) (int, error) {
	if e := validateForwardConfig(f); e != nil {
		return 2, e
	}
	return startResourceBackground(resourceConfig{Forward: &f}, "")
}

// Reconnect only an already-established, explicitly opted-in local forward.
// Each retry owns a new transport; application TCP connections are not resumed.
func runForward(parent context.Context, f forwardConfig, s *resourceService, onReady func() error) (int, error) {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	s.record.Detail = f.description("local", f.Host)
	advertisement, err := json.Marshal(s.peerTask(f.description("manager", "here")))
	if err != nil {
		return 1, err
	}
	advertisement = append(advertisement, '\n')
	attempt := func(initial bool) (bool, error) {
		var ready func() error
		if initial {
			ready = onReady
		}
		return runForwardAttempt(ctx, s, advertisement, ready, func(ctx context.Context, diagnostic io.Writer) (*forwardConnection, error) {
			return startForwardConnection(ctx, f, s, initial, diagnostic)
		})
	}
	return retryForward(ctx, f.Retries, s, attempt)
}

// retryForward owns retry policy; an attempt owns the underlying connection.
func retryForward(ctx context.Context, retries int, s *resourceService, attempt func(bool) (bool, error)) (int, error) {
	established, err := attempt(true)
	for retry := 1; established && retry <= retries && ctx.Err() == nil; retry++ {
		s.setTransport("reconnecting", err)
		delay := time.Second * time.Duration(1<<min(retry-1, 5))
		termui.Log(os.Stderr, taskShortCode(s.record.UID), termui.Info, "Forward disconnected; TCP connections lost. Retry %d/%d in %s.", retry, retries, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, nil
		case <-timer.C:
		}
		_, err = attempt(false)
	}
	if ctx.Err() != nil {
		return 0, nil
	}
	return 1, err
}

func startForwardConnection(ctx context.Context, f forwardConfig, s *resourceService, initial bool, diagnostic io.Writer) (*forwardConnection, error) {
	if f.Connection != nil {
		return startRoutedForward(ctx, f)
	}
	connection := forwardSSHOptions(f, forwardRemoteCommand(f))
	connection.Environment, connection.Directory = s.baseEnvironment(), s.directory
	if initial {
		connection.Confirm, connection.Secret = s.config.Auth.callbacks()
		connection.Warn = s.config.Auth.warn
	} else {
		connection.RequireKnownHost = true
	}
	connection.Diagnostic = diagnostic
	client, close, err := openDirectForward(ctx, connection, f)
	if err != nil {
		return nil, err
	}
	process, err := client.Start(connection.Command, diagnostic)
	if err != nil {
		close()
		return nil, err
	}
	return &forwardConnection{Reader: process, Writer: process, Wait: process.Wait, close: func() error { close(); return nil }}, nil
}

var errForwardRetries = errors.New("--retries must be between 0 and 10")

var errForwardPort = errors.New("port must be an integer from 1 to 65535")

func parseForwardPort(s string) (int, error) {
	if s == "" {
		return 0, errForwardPort
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errForwardPort
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, errForwardPort
	}
	return n, nil
}

// Validate checks port and retry values before task identity checks.
func (c forwardConfig) Validate() error {
	if !validRetries(c.Retries) {
		return errForwardRetries
	}
	if c.Port < 1 || c.Port > 65535 || c.Target < 1 || c.Target > 65535 {
		return fmt.Errorf("invalid forward ports")
	}
	return nil
}

// SSH resolves connection settings; the application binds its own forwarding rule.
func forwardSSHOptions(c forwardConfig, command string) sshio.Options {
	return sshio.Options{Host: c.Host, Command: command, Args: c.SSH}
}

// openDirectForward owns both the SSH connection and its forwarding workers.
// The listener must be established before starting the remote readiness helper.
func openDirectForward(ctx context.Context, options sshio.Options, rule forwardConfig) (*sshio.Client, func(), error) {
	if err := rule.Validate(); err != nil {
		return nil, nil, err
	}
	client, err := sshio.Dial(ctx, options)
	if err != nil {
		return nil, nil, err
	}
	// Remote listener setup may block waiting for an SSH reply.
	stop := context.AfterFunc(ctx, func() { client.Close() })
	worker, err := startDirectForward(client, rule)
	if err == nil {
		err = ctx.Err()
	}
	close := sync.OnceFunc(func() {
		stop()
		client.Close()
		if worker != nil {
			worker.Close()
		}
	})
	if err != nil {
		close()
		return nil, nil, err
	}
	return client, close, nil
}

func startDirectForward(c *sshio.Client, rule forwardConfig) (*forwardio.Forwarder, error) {
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
		if rule.Reverse && err.Error() == "ssh: tcpip-forward request denied by peer" {
			return nil, fmt.Errorf("forward %s: %w; the SSH server did not provide a reason; check its TCP forwarding policy and whether the remote port is already in use", listen, err)
		}
		return nil, fmt.Errorf("forward %s: %w", listen, err)
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
	return forwardio.StartForwardListener(listener, dial), nil
}

// Foreground forwards own their connection until explicit cancellation or loss.
func runForwardTransportForeground(ctx context.Context, c forwardConfig, diagnostic io.Writer) (int, error) {
	options := forwardSSHOptions(c, "")
	options.Confirm, options.Secret = confirmSSHHost, promptSSHSecret
	options.Diagnostic = diagnostic
	connection, close, err := openDirectForward(ctx, options, c)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 1, err
	}
	defer close()
	err = connection.Wait()
	if ctx.Err() != nil {
		return 0, nil
	}
	return 1, fmt.Errorf("forward disconnected: %w", err)
}

// parseForwardRetries applies the same retry policy to CLI and persisted configuration.
func parseForwardRetries(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || !validRetries(n) {
		return 0, errForwardRetries
	}
	return n, nil
}

func validRetries(n int) bool { return n >= 0 && n <= 10 }

const forwardReadyMarker = "sshore-forward-ready"

type forwardConnection struct {
	io.Reader
	io.Writer
	Wait  func() error
	close func() error
}

// Close interrupts both stream I/O and Wait.
func (c *forwardConnection) Close() error { return c.close() }

func runForwardAttempt(parent context.Context, s *resourceService, advertisement []byte, onReady func() error, open func(context.Context, io.Writer) (*forwardConnection, error)) (bool, error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	var diagnostics diagnosticsTail
	connection, err := open(ctx, io.MultiWriter(s.logWriter(), &diagnostics))
	if err != nil {
		return false, err
	}
	closeConnection := sync.OnceFunc(func() { _ = connection.Close() })
	stop := context.AfterFunc(ctx, closeConnection)
	var heartbeat sync.WaitGroup
	defer func() {
		cancel(nil)
		stop()
		closeConnection()
		heartbeat.Wait()
	}()
	// Read only the bounded protocol marker. Unexpected login output is an error.
	marker, readErr := bufio.NewReader(io.LimitReader(connection, int64(len(forwardReadyMarker)+2))).ReadString('\n')
	startupErr := readErr
	switch {
	case readErr != nil:
	case marker != forwardReadyMarker+"\n":
		startupErr = fmt.Errorf("unexpected forward startup response: %q", termui.CleanText(marker))
	default:
		s.setTransport("connected", nil)
		if onReady != nil {
			startupErr = onReady()
		}
	}
	if startupErr != nil {
		// A failed handshake can leave the peer waiting for input.
		cancel(startupErr)
	} else {
		heartbeat.Go(func() {
			activity := accountActivity.Load()
			if activity == nil {
				cancel(errors.New("forward heartbeat requires the account daemon"))
				return
			}
			// The registered forward already keeps the account daemon busy.
			pulse := activity.heartbeat()
			for ctx.Err() == nil {
				if err := writeFull(connection, advertisement); err != nil {
					cancel(fmt.Errorf("forward heartbeat write failed: %w", err))
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-pulse:
					pulse = activity.heartbeat()
				}
			}
		})
	}

	err = connection.Wait()
	if readErr != nil {
		detail := diagnostics.String()
		if detail == "" {
			if err == nil {
				err = readErr
			}
			detail = err.Error()
		}
		return false, fmt.Errorf("forward startup failed (%s): %s", s.record.Detail, detail)
	}
	if startupErr != nil {
		return false, startupErr
	}
	if cause := context.Cause(ctx); cause != nil {
		return true, cause
	}
	if err == nil {
		err = errors.New("forward transport closed")
	}
	return true, err
}

// Keep only the current attempt's diagnostic tail, never earlier log entries.
type diagnosticsTail struct {
	mu   sync.Mutex
	data []byte
}

func (d *diagnosticsTail) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(p)
	if len(p) >= 4096 {
		d.data = append(d.data[:0], p[len(p)-4096:]...)
	} else {
		if len(d.data)+len(p) > 4096 {
			d.data = d.data[len(d.data)+len(p)-4096:]
		}
		d.data = append(d.data, p...)
	}
	return n, nil
}
func (d *diagnosticsTail) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return termui.CleanText(strings.TrimSpace(string(d.data)))
}

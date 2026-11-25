package main

import (
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

// Both entry points mirror SOURCE on TARGET. Only the client endpoint may
// omit its port, and a remote request can only suggest that client port.
func parseForwardEndpoints(args []string, attached bool) (string, operation, error) {
	op := operation{Kind: "forward"}
	if len(args) != 2 {
		return "", op, usage("forward requires SOURCE TARGET")
	}
	source, sourcePort, err := splitPortEndpoint(args[0])
	if err != nil {
		return "", op, err
	}
	target, targetPort, err := splitPortEndpoint(args[1])
	if err != nil {
		return "", op, err
	}
	client := "here"
	if attached {
		client = "origin"
	} else if source == "origin" || target == "origin" {
		return "", op, usage("origin: requires an attached sshore shell")
	}
	op.Reverse = source == client
	host, remotePort, localPort := source, sourcePort, targetPort
	if op.Reverse {
		host, remotePort, localPort = target, targetPort, sourcePort
	}
	if attached && host != "here" {
		return "", op, usage("forward requires one here:PORT and one origin:[PORT]")
	}
	if (source == client) == (target == client) {
		return "", op, usage("forward requires one client endpoint and one remote endpoint")
	}
	port, err := parsePort(remotePort)
	if err != nil {
		return "", op, err
	}
	if op.Reverse {
		op.Port = port
	} else {
		op.TargetPort = port
	}
	if localPort != "" {
		port, err = parsePort(localPort)
		if err != nil {
			return "", op, err
		}
		if attached {
			op.LocalHint = localPort
		} else {
			resolveLocalPort(&op, port)
		}
	}
	return host, op, nil
}

func parseForwardCommand(parsed cliArguments) (forwardConfig, error) {
	var f forwardConfig
	f.SSH = parsed.ssh
	f.Retries = parsed.integer("--retries")
	host, op, err := parseForwardEndpoints(parsed.operands, false)
	f.Host, f.Port, f.Target, f.Reverse = host, op.Port, op.TargetPort, op.Reverse
	return f, err
}

func localForwardPortLabel(reverse bool) string {
	if reverse {
		return "Local source port"
	}
	return "Local target port"
}

func (f forwardConfig) direction() string {
	if f.Reverse {
		return "->"
	}
	return "<-"
}

func (f forwardConfig) description(client, remote string) string {
	source, target := remote, client
	if f.Reverse {
		source, target = client, remote
	}
	return sshio.FormatEndpoint(source, strconv.Itoa(f.Target)) + " -> " +
		sshio.FormatEndpoint(target, strconv.Itoa(f.Port))
}

// Remote helper selection is application policy; the engine receives opaque command text.
func forwardRemoteCommand(f forwardConfig) string {
	req := remoteServiceRequest{Operation: "forward-agent", Forward: &operation{Kind: "forward", Port: f.Port, TargetPort: f.Target, Reverse: f.Reverse}}
	return renderRemoteCommand(f.Server, req)
}

func pendingPortDescription(op operation) string {
	if op.Reverse {
		return fmt.Sprintf("Mirror a local service on remote port %s", forwardio.Loopback(op.ServerPort()))
	}
	return fmt.Sprintf("Mirror remote service %s on a local port", forwardio.Loopback(op.ServerPort()))
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
		if f.Connection.Instance != f.Instance || f.Connection.Session != f.Session || f.Connection.Host != f.Host || f.ApprovalID == "" {
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
		if f.Connection == nil || !validTaskUID(f.Instance) || !validRequestID(f.ApprovalID) || validSession(f.Session) != nil || f.ResumePID <= 0 || f.Retries != 0 {
			return fmt.Errorf("invalid approved forward identity")
		}
	}
	return transportErr
}
func startForwardSession(f forwardConfig) (int, error) {
	if e := validateForwardConfig(f); e != nil {
		return 2, e
	}
	return startResourceBackground(resourceConfig{Forward: &f})
}

// Reconnect only an already-established, explicitly opted-in local forward.
// Each retry owns a new transport; application TCP connections are not resumed.
func runForward(parent context.Context, f forwardConfig, s *resourceService, onReady func() error) (int, error) {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := "local"
	if f.Connection != nil {
		client = "origin"
	}
	s.record.Detail = f.description(client, f.Host)
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
		return runForwardAttempt(ctx, s, advertisement, ready, func(ctx context.Context, diagnostic io.Writer) (*forwardio.Session, error) {
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
		termui.Log(s.logWriter(), canonicalTaskCode(resourceIdentity(s.record)), termui.Info, "Forward disconnected; TCP connections lost. Retry %d/%d in %s.", retry, retries, delay)
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

func startForwardConnection(ctx context.Context, f forwardConfig, s *resourceService, initial bool, diagnostic io.Writer) (*forwardio.Session, error) {
	if f.Connection != nil {
		request := message{Type: "route_forward", Name: f.Session, PID: f.ResumePID, ApprovalID: f.ApprovalID,
			Operations: []operation{{Kind: "forward", Port: f.Port, TargetPort: f.Target, Reverse: f.Reverse}}}
		return startForward(ctx, f, func() (io.ReadWriteCloser, error) {
			return openIndependentResource(ctx, *f.Connection, request, s)
		})
	}
	connection := forwardSSHOptions(f, forwardRemoteCommand(f))
	connection.Environment, connection.Directory = s.baseEnvironment(), s.directory
	var auth *sshAuthEndpoint
	if initial {
		auth = s.config.Auth
		connection.Confirm, connection.Secret = auth.callbacks()
	} else {
		connection.RequireKnownHost = true
	}
	connection = withSSHDiagnostics(connection, diagnostic, auth)
	return startSSHForward(ctx, f, connection, diagnostic)
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

func startSSHForward(ctx context.Context, f forwardConfig, config sshio.Options, diagnostic io.Writer) (*forwardio.Session, error) {
	return startForward(ctx, f, func() (io.ReadWriteCloser, error) {
		conn, err := sshio.Start(ctx, config, diagnostic)
		if err != nil {
			return nil, err
		}
		reply, err := readMessage(conn)
		if err == nil {
			err = checkPeerVersion(reply.SoftwareVersion, "forward service")
		}
		if err == nil && reply.Type != "resource_ready" {
			err = fmt.Errorf("forward service: %s", cleanText(reply.Error))
		}
		if err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	})
}

func startForward(ctx context.Context, f forwardConfig, open func() (io.ReadWriteCloser, error)) (*forwardio.Session, error) {
	if f.Reverse {
		return forwardio.Start(ctx, 0, f.Target, true, open, nil)
	}
	return forwardio.Start(ctx, f.Port, 0, true, open, nil)
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

func runForwardAttempt(parent context.Context, s *resourceService, advertisement []byte, onReady func() error, open func(context.Context, io.Writer) (*forwardio.Session, error)) (bool, error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	var diagnostics diagnosticsTail
	log := &logStream{out: s.logWriter(), id: canonicalTaskCode(resourceIdentity(s.record)), level: termui.Error}
	defer log.flush()
	connection, err := open(ctx, io.MultiWriter(log, &diagnostics))
	if err != nil {
		detail := diagnostics.String()
		if detail == "" {
			detail = err.Error()
		}
		return false, fmt.Errorf("forward startup failed (%s): %s", s.record.Detail, detail)
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
	s.setTransport("connected", nil)
	var startupErr error
	if onReady != nil {
		startupErr = onReady()
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

func runDaemonForward(config resourceConfig, enc *json.Encoder, dec *json.Decoder, c net.Conn, request localDaemonRequest, tasks *daemonTasks) error {
	f := *config.Forward
	if err := validateForwardConfig(f); err != nil {
		return err
	}
	record := resourceRecord{Kind: "forward", Name: f.Name, Host: f.Host, Direction: f.direction()}
	s := newResourceService(config, record)
	s.log = tasks.log
	s.environment, s.directory = request.Env, request.Record.Directory
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task := &daemonResourceTask{record: s.record, done: s.done, control: func(action string) resourceReply {
		switch action {
		case "kill":
			cancel()
			return resourceReply{}
		case "status":
			return resourceReply{Status: s.statusSnapshot()}
		default:
			return resourceReply{Error: "invalid resource operation"}
		}
	}}
	if err := tasks.register(config.UID, task); err != nil {
		return err
	}
	s.record.Name = task.taskName()
	ack := awaitResourceCommit(dec.Decode, cancel)
	_, err := runForward(ctx, f, s, func() error {
		record := task.publishReady(s.record.Detail, "", "")
		if err := offerResourceStartup(enc, record, ack, nil); err != nil {
			return err
		}
		acknowledgeResourceStartup(enc, c)
		return nil
	})
	task.finish(s.done)
	return err
}

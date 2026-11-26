package main

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
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
	if !validRetries(f.Retries) {
		return usage("%s", errForwardRetries)
	}
	if f.Name != "" {
		if err := validSession(f.Name); err != nil {
			return err
		}
	}
	if err := validSSHDestination(f.Host); err != nil {
		return err
	}
	if f.Connection != nil || f.ApprovalID != "" || f.Session != "" || f.ResumePID != 0 {
		if f.Connection == nil || !validTaskUID(f.Instance) || !validRequestID(f.ApprovalID) ||
			validSession(f.Session) != nil || f.ResumePID <= 0 || f.Retries != 0 {
			return fmt.Errorf("invalid approved forward identity")
		}
		if err := validateRecentConnection(*f.Connection); err != nil {
			return err
		}
		if f.Connection.Instance != f.Instance || f.Connection.Session != f.Session || f.Connection.Host != f.Host {
			return fmt.Errorf("invalid routed forward approval")
		}
	}
	return validateForwardPorts(f.Port, f.Target)
}

func startForwardSession(f forwardConfig) (int, error) {
	if e := validateForwardConfig(f); e != nil {
		return 2, e
	}
	return startResourceBackground(resourceConfig{Forward: &f})
}

// Reconnect only an already-established, explicitly opted-in local forward.
// Each retry owns a new transport; application TCP connections are not resumed.
func runForward(ctx context.Context, f forwardConfig, s *resourceService, onReady func() error) error {
	client := "local"
	if f.Connection != nil {
		client = "origin"
	}
	s.record.Detail = f.description(client, f.Host)
	advertisement, err := json.Marshal(s.peerTask(f.description("manager", "here")))
	if err != nil {
		return err
	}
	advertisement = append(advertisement, '\n')
	for retry := 0; ; retry++ {
		if retry != 0 {
			s.setTransport("reconnecting", err)
			delay := time.Second * time.Duration(1<<min(retry-1, 5))
			termui.Log(s.logWriter(), canonicalTaskCode(resourceIdentity(s.record)), termui.Info,
				"Forward disconnected; TCP connections lost. Retry %d/%d in %s.", retry, f.Retries, delay)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
		var established bool
		established, err = runForwardAttempt(ctx, f, s, retry == 0, advertisement, onReady)
		if ctx.Err() != nil {
			return nil
		}
		if retry == f.Retries || retry == 0 && !established {
			return err
		}
	}
}

func startForwardConnection(ctx context.Context, f forwardConfig, s *resourceService, initial bool, diagnostic io.Writer) (*forwardio.Session, error) {
	listen, target := f.Port, 0
	if f.Reverse {
		listen, target = 0, f.Target
	}
	return forwardio.Start(ctx, listen, target, true, func() (io.ReadWriteCloser, error) {
		if f.Connection != nil {
			request := message{Type: "route_forward", Name: f.Session, PID: f.ResumePID, ApprovalID: f.ApprovalID,
				Operations: []operation{{Kind: "forward", Port: f.Port, TargetPort: f.Target, Reverse: f.Reverse}}}
			return openIndependentResource(ctx, *f.Connection, request, s)
		}
		connection := remoteService(sshio.Options{
			Host: f.Host, Args: f.SSH, Environment: s.baseEnvironment(), Directory: s.directory,
			RequireKnownHost: !initial,
		}, f.Server, remoteServiceRequest{
			Operation: "forward-agent",
			Forward:   &remoteForwardRequest{Kind: "forward", Port: f.Port, TargetPort: f.Target, Reverse: f.Reverse},
		})
		var auth *sshAuthEndpoint
		if initial {
			auth = s.config.Auth
			connection.Confirm, connection.Secret = auth.callbacks()
		}
		conn, err := sshio.Start(ctx, withSSHDiagnostics(connection, diagnostic, auth), diagnostic)
		if err != nil {
			return nil, err
		}
		reply, err := readMessage(conn)
		if err == nil {
			err = reply.peerError("forward service")
		}
		if err == nil && reply.Type != "resource_ready" {
			err = fmt.Errorf("unexpected forward setup response: %s", reply.Type)
		}
		if err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}, nil)
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

func validateForwardPorts(port, target int) error {
	if port < 1 || port > 65535 || target < 1 || target > 65535 {
		return fmt.Errorf("invalid forward ports")
	}
	return nil
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

func runForwardAttempt(parent context.Context, f forwardConfig, s *resourceService, initial bool, advertisement []byte, onReady func() error) (bool, error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	var diagnostics diagnosticsTail
	log := termui.NewLogWriter(s.logWriter(), canonicalTaskCode(resourceIdentity(s.record)), termui.Error)
	defer log.Flush()
	connection, err := startForwardConnection(ctx, f, s, initial, io.MultiWriter(log, &diagnostics))
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if detail := diagnostics.String(); detail != "" {
				err = errors.New(detail)
			}
		}
		return false, fmt.Errorf("forward startup failed (%s): %w", s.record.Detail, err)
	}
	defer connection.Close()
	s.setTransport("connected", nil)
	if initial && onReady != nil {
		if err := onReady(); err != nil {
			return false, err
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pulse := time.NewTicker(daemonHeartbeatInterval)
		defer pulse.Stop()
		for ctx.Err() == nil {
			if err := writeFull(connection, advertisement); err != nil {
				cancel(fmt.Errorf("forward heartbeat write failed: %w", err))
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-pulse.C:
			}
		}
	}()
	connection.Wait()
	cause := context.Cause(ctx)
	cancel(nil)
	<-done
	if cause != nil {
		return true, cause
	}
	return true, errors.New("forward transport closed")
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

func runDaemonForward(config resourceConfig, enc *jsontext.Encoder, dec *jsontext.Decoder, c net.Conn, request localDaemonRequest, tasks *daemonTasks) error {
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
	ack := awaitResourceCommit(func(v any) error { return json.UnmarshalDecode(dec, v) }, cancel)
	err := runForward(ctx, f, s, func() error {
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

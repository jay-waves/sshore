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

	"github.com/jay-waves/sshore/internal/ports"
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
func parseForwardEndpoints(args []string, attached bool) (string, ports.Rule, error) {
	var rule ports.Rule
	if len(args) != 2 {
		return "", rule, usage("forward requires SOURCE TARGET")
	}
	source, sourcePort, err := splitPortEndpoint(args[0])
	if err != nil {
		return "", rule, err
	}
	target, targetPort, err := splitPortEndpoint(args[1])
	if err != nil {
		return "", rule, err
	}
	client := "here"
	if attached {
		client = originLabel
	} else if source == "origin" || target == "origin" {
		return "", rule, usage("..: requires an attached sshore shell")
	}
	rule.Reverse = source == client
	host, remotePort, localPort := source, sourcePort, targetPort
	if rule.Reverse {
		host, remotePort, localPort = target, targetPort, sourcePort
	}
	if attached && host != "here" {
		return "", rule, usage("forward requires one .:PORT and one ..:[PORT]")
	}
	if (source == client) == (target == client) {
		return "", rule, usage("forward requires one client endpoint and one remote endpoint")
	}
	rule.RemotePort, err = parsePort(remotePort)
	if err != nil {
		return "", rule, err
	}
	if localPort != "" {
		rule.LocalPort, err = parsePort(localPort)
		if err != nil {
			return "", rule, err
		}
	}
	return host, rule, nil
}

func localForwardPortLabel(reverse bool) string {
	if reverse {
		return "Local source port"
	}
	return "Local target port"
}

func (f forwardConfig) direction() string {
	if f.Rule.Reverse {
		return "->"
	}
	return "<-"
}

func (f forwardConfig) description(client, remote string) string {
	local := formatCLIEndpoint(client, strconv.Itoa(f.Rule.LocalPort))
	host := formatCLIEndpoint(remote, strconv.Itoa(f.Rule.RemotePort))
	if !f.Rule.Reverse {
		local, host = host, local
	}
	mapping := local + " -> " + host
	if f.Rule.UDP {
		return "UDP " + mapping
	}
	return mapping
}

func validateForwardProposal(op operation, count int) error {
	if count != 1 || op.LocalHint != "" || op.Path != "" || op.Name != "" || op.Size != 0 ||
		op.Forward.Validate(true) != nil {
		return fmt.Errorf("invalid port request")
	}
	return nil
}

func pendingForwardDescription(r ports.Rule) string {
	service := "service"
	if r.UDP {
		service = "UDP service"
	}
	if r.Reverse {
		return fmt.Sprintf("Mirror a local %s on remote port %s", service, ports.Loopback(r.RemotePort))
	}
	return fmt.Sprintf("Mirror remote %s %s on a local port", service, ports.Loopback(r.RemotePort))
}

type forwardConfig struct {
	Rule       ports.Rule
	Connection *recentConnection
	Retries    int
	ApprovalID string
	ResumePID  int
	Host, Name string
	SSH        []string
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
	if f.Connection != nil || f.ApprovalID != "" || f.ResumePID != 0 {
		if f.Connection == nil || !validTaskUID(f.Connection.Instance) || !validRequestID(f.ApprovalID) ||
			f.ResumePID <= 0 || f.Retries != 0 || f.Connection.Host != f.Host {
			return fmt.Errorf("invalid approved forward identity")
		}
		if err := validateRecentConnection(*f.Connection); err != nil {
			return err
		}
	}
	return f.Rule.Validate(false)
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
		client = originLabel
	}
	s.record.Detail = f.description(client, f.Host)
	advertisement, err := json.Marshal(s.peerTask(f.description(originLabel, "here")))
	if err != nil {
		return err
	}
	advertisement = append(advertisement, '\n')
	for retry := 0; ; retry++ {
		if retry != 0 {
			delay := time.Second * time.Duration(1<<min(retry-1, 5))
			termui.Log(s.logWriter(), canonicalTaskCode(taskKey(s.record.UID)), termui.Info,
				"Forward disconnected; active flows lost. Retry %d/%d in %s.", retry, f.Retries, delay)
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

func startForwardConnection(ctx context.Context, f forwardConfig, s *resourceService, initial bool, diagnostic io.Writer) (*ports.Session, error) {
	return f.Rule.Start(ctx, true, func() (io.ReadWriteCloser, error) {
		if f.Connection != nil {
			request := approvedResourceRequest(f.Connection.Session, f.ResumePID, f.ApprovalID, operation{Kind: "forward", Forward: f.Rule})
			return openIndependentResource(ctx, *f.Connection, request, s)
		}
		connection := remoteService(sshio.Options{
			Host: f.Host, Args: f.SSH, Environment: s.baseEnvironment(), Directory: s.directory,
			RequireKnownHost: !initial,
		}, remoteServiceRequest{
			Operation: "forward-agent",
			Forward:   &f.Rule,
		})
		var auth *sshAuthEndpoint
		if initial {
			auth = s.auth
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
	log := termui.NewLogWriter(s.logWriter(), canonicalTaskCode(taskKey(s.record.UID)), termui.Error)
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
	record := resourceRecord{UID: config.UID, Kind: "forward", Name: f.Name, Host: f.Host, Direction: f.direction()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task, err := tasks.registerResource(record, cancel)
	if err != nil {
		return err
	}
	s := &resourceService{record: task.record, auth: config.Auth, log: tasks.log, environment: request.Env, directory: request.Record.Directory}
	ack := awaitResourceCommit(func(v any) error { return json.UnmarshalDecode(dec, v) }, cancel)
	err = runForward(ctx, f, s, func() error {
		record := tasks.resourceReady(task, s.record.Detail, "", "")
		if err := offerResourceStartup(enc, record, ack, nil); err != nil {
			return err
		}
		acknowledgeResourceStartup(enc, c)
		return nil
	})
	tasks.finishResource(task)
	return err
}

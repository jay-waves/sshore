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

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/wire"
)

func parsePort(s string) (int, error) {
	n, err := parseForwardPort(s)
	if err != nil {
		return 0, usage("%s", err)
	}
	return n, nil
}

// A qualified endpoint identifies the peer; the first endpoint always listens.
func parseForwardEndpoints(args []string) (string, int, int, bool, error) {
	fail := func() (string, int, int, bool, error) {
		return "", 0, 0, false, usage("invalid port mapping; use forward self:[PORT] HOST:PORT or forward HOST:PORT self:[PORT]")
	}
	if len(args) != 2 {
		return fail()
	}
	endpoint := func(value string) (string, int, error) {
		host, port, err := splitEndpoint(value)
		if err != nil {
			return "", 0, err
		}
		if host == "origin" {
			return "", 0, usage("origin: requires an attached sshore shell")
		}
		if host == "self" {
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
	leftHost, leftPort, err := splitEndpoint(args[0])
	if err != nil {
		return op, err
	}
	rightHost, rightPort, err := splitEndpoint(args[1])
	if err != nil {
		return op, err
	}
	var remote string
	switch {
	case leftHost == "origin" && rightHost == "self":
		op.LocalHint, remote = leftPort, rightPort
	case leftHost == "self" && rightHost == "origin":
		remote, op.LocalHint, op.Reverse = leftPort, rightPort, true
	default:
		return op, usage("forward between self:PORT and origin:[PORT]")
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

func describeForward(port, target int, reverse bool) string {
	listener, service := "your computer", "server"
	if reverse {
		listener, service = service, listener
	}
	return fmt.Sprintf("%s %s -> %s %s", listener, loopback(port), service, loopback(target))
}

func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// Local requests default to independent background workers; --foreground is
// available for diagnostics. Remote shell
// requests inherit the session and require local approval. Port commands resolve
// server/local ports into fixed listener and target identities. Native SSH
// configuration/authentication remains effective.

// Remote helper selection is application policy; the engine receives opaque command text.
func forwardRemoteCommand(f forwardConfig) string {
	if f.ApprovalID != "" {
		return approvedForwardCommand(f)
	}
	return forwardWatchCommand(f.Host, f.Server)
}

func runForwardForeground(f forwardConfig) (int, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	fmt.Fprintf(os.Stderr, "Starting %s (%s); Ctrl-C stops it.\n", describeForward(f.Port, f.Target, f.Reverse), cleanText(f.Host))
	return runForwardTransportForeground(ctx, sshExecutable(), f, os.Stdin, os.Stdout, os.Stderr)
}

func runEndpointForward(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		if _, err := remoteForwardRequest(args); err != nil {
			return 2, err
		}
		return 0, serverCommand(append([]string{"forward"}, args...))
	}
	f, foreground, err := parseForwardCommand(args)
	if err != nil {
		return 2, err
	}
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	if f.Port == 0 || f.Target == 0 {
		input, err := newLocalInput()
		if err != nil {
			return 2, err
		}
		defer input.Close()
		label := "Local listening port"
		remote := f.Target
		if f.Reverse {
			label = "Local service port"
			remote = f.Port
		}
		termui.Log(os.Stdout, termui.Info, "Remote %s:%d. Submit local port to forward.\n", cleanText(f.Host), remote)
		port, err := readSelectedPort(input, label, "", "forward cancelled")
		if err != nil {
			return 1, err
		}
		if f.Reverse {
			f.Target = port
		} else {
			f.Port = port
		}
		if err = input.Close(); err != nil {
			return 1, err
		}
	}
	if foreground {
		return runForwardForeground(f)
	}
	return startForwardSession(f)
}

func pendingPortDescription(op operation) string {
	if op.Reverse {
		return fmt.Sprintf("server %s -> local service (choose port in review)", loopback(op.ServerPort()))
	}
	return fmt.Sprintf("local listener (choose port in review) -> server %s", loopback(op.ServerPort()))
}

func resolveLocalPort(op *operation, port int) {
	if op.Reverse {
		op.TargetPort = port
	} else {
		op.Port = port
	}
	op.LocalHint = ""
}

// Independent forwards use an owned SSH connection and a transient remote Unix
// helper, never a shell session daemon. OpenSSH's forwarding failure gate runs
// before the helper command, whose fixed marker acknowledges startup. Killing a
// forward closes only this transport. Approved forwards require the existing
// remote executable to validate the source session; no installation is attempted.

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
	return startResourceBackground(resourceConfig{Forward: &f}, "forward")
}

// Reconnect only an already-established, explicitly opted-in local forward.
// Each retry owns a new transport; application TCP connections are not resumed.
func runForward(f forwardConfig, s *resourceService) (int, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var stopped chan error
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case stopped = <-s.stop:
			cancel()
		case <-s.abandoned:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		cancel()
		<-watcherDone
		if stopped != nil {
			stopped <- nil
		}
	}()
	return runForwardTransport(ctx, forwardTransport{
		Retries:     f.Retries,
		Description: describeForward(f.Port, f.Target, f.Reverse),
		Log:         s.logWriter(),
		Start: func(ctx context.Context, initial bool, diagnostic io.Writer) (*forwardConnection, error) {
			return startForwardConnection(ctx, f, s, initial, diagnostic)
		},
		Ready: func() error {
			s.record.Detail = describeForward(f.Port, f.Target, f.Reverse)
			return s.ready("", "")
		},
		Status: s.setTransport,
		Heartbeat: func() ([]byte, error) {
			data, err := json.Marshal(s.peerTask())
			return append(data, '\n'), err
		},
	})
}

func startForwardConnection(ctx context.Context, f forwardConfig, s *resourceService, initial bool, diagnostic io.Writer) (*forwardConnection, error) {
	if f.Connection != nil {
		return startRoutedForward(ctx, f)
	}
	connection := forwardSSHOptions(f, forwardRemoteCommand(f))
	connection.Environment, connection.Directory = s.sshEnvironment(), s.directory
	if initial {
		connection.Confirm = brokerSSHConfirmation(connection.Environment)
		connection.Secret = brokerSSHSecret(connection.Environment)
	} else {
		connection.Args = append([]string{"-o", "StrictHostKeyChecking=yes"}, connection.Args...)
	}
	process, err := sshio.Start(ctx, connection, diagnostic)
	if err != nil {
		return nil, err
	}
	return &forwardConnection{Input: process.Stdin(), Output: process, Wait: process.Wait, Close: process.Close}, nil
}

func forwardWatchCommand(host, server string) string {
	return forwardHelperCommand(host, server) + "printf '%s\\n' " + shellQuote(forwardReadyMarker) +
		"; \"$server\" task-watch " + shellQuote(version) + " || exec cat >/dev/null"
}

func approvedForwardCommand(f forwardConfig) string {
	command := forwardHelperCommand(f.Host, f.Server) + "exec \"$server\" forward-watch " + shellQuote(version) + " " + shellQuote(f.Session) + fmt.Sprintf(" %d ", f.ResumePID) + shellQuote(f.ApprovalID)
	if f.Instance != "" {
		command += " " + shellQuote(f.Instance)
	}
	return command
}

func forwardHelperCommand(host, server string) string {
	if server == "" {
		server = "sshore"
	}
	command := "unset SSHORE_SOCKET SSHORE_CONFIRM_ADDR SSHORE_CONFIRM_TOKEN; server=" + shellQuote(server) + `; if command -v "$server" >/dev/null 2>&1; then server=$(command -v "$server"); `
	if server == "sshore" {
		command += `else server="$HOME/.local/bin/sshore"; `
	}
	return command + `fi; if [ "$("$server" --version 2>/dev/null)" != ` + shellQuote("sshore "+version) +
		` ]; then printf '%s\n' ` + shellQuote(remoteVersionHelp(host)) + ` >&2; exit 1; fi; `
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

// Pass the owned loopback rule directly; ssh -G resolves authentication and
// transport settings without serializing and parsing the product's ports.
func forwardSSHOptions(c forwardConfig, command string) sshio.Options {
	args := []string{"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
	args = append(args, sshio.WithoutForwarding(c.SSH)...)
	return sshio.Options{Host: c.Host, Command: command, Args: args,
		Forward: &sshio.Forward{Port: c.Port, Target: c.Target, Reverse: c.Reverse}}
}

// Foreground forwards own their connection until explicit cancellation or loss.
func runForwardTransportForeground(ctx context.Context, executable string, c forwardConfig, in io.Reader, out, diagnostic io.Writer) (int, error) {
	options := forwardSSHOptions(c, "")
	options.Executable, options.Confirm, options.Secret = executable, confirmSSHHost, promptSSHSecret
	connection, err := sshio.Dial(ctx, options)
	if err != nil {
		return 1, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	if err := connection.StartForwarding(); err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 1, err
	}
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

// forwardTransport joins SSH attempts, readiness, heartbeat and cancellation.
// Command returns a fresh, context-bound command; all hooks and Log are required.
// Heartbeat returns a complete wire record once per attempt, after readiness.
// The caller owns its schema; the engine writes the bytes unchanged.
type forwardTransport struct {
	Retries     int
	Description string
	Log         io.Writer
	Start       func(context.Context, bool, io.Writer) (*forwardConnection, error)
	Ready       func() error
	Status      func(string, error)
	Heartbeat   func() ([]byte, error)
}

// runForwardTransport retries established forwards only. Cancellation closes this transport,
// while existing application TCP connections are never replayed.
func runForwardTransport(ctx context.Context, runtime forwardTransport) (int, error) {
	started := false
	for attempt := 0; ; attempt++ {
		code, err := runAttempt(ctx, runtime, attempt == 0, &started)
		if ctx.Err() != nil {
			return 0, nil
		}
		if err == nil || attempt >= runtime.Retries || !started {
			return code, err
		}
		runtime.Status("reconnecting", err)
		delay := retryDelay(attempt)
		fmt.Fprintf(runtime.Log, "[sshore] Forward disconnected; TCP connections lost. Retry %d/%d in %s.\n", attempt+1, runtime.Retries, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, nil
		case <-timer.C:
		}
	}
}
func retryDelay(attempt int) time.Duration {
	return time.Second * time.Duration(1<<min(attempt, 5))
}

type forwardConnection struct {
	Input  io.WriteCloser
	Output io.Reader
	Wait   func() error
	Close  func() error
}

func runAttempt(parent context.Context, runtime forwardTransport, initial bool, started *bool) (int, error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	var diagnostics diagnosticsTail
	connection, e := runtime.Start(ctx, initial, io.MultiWriter(runtime.Log, &diagnostics))
	if e != nil {
		return 1, e
	}
	input, output := connection.Input, connection.Output
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = connection.Wait(); close(done) }()
	var heartbeat sync.WaitGroup
	stop := sync.OnceFunc(func() {
		cancel(nil)
		// Abort before joining: stdin.Close can wait behind a blocked write.
		connection.Close()
		<-done
		heartbeat.Wait()
	})
	defer stop()
	// Read only the bounded protocol marker. Unexpected login output is an error.
	marker, e := bufio.NewReader(io.LimitReader(output, int64(len(forwardReadyMarker)+2))).ReadString('\n')
	if e != nil {
		stop()
		detail := diagnostics.String()
		if detail == "" {
			detail = fmt.Sprint(waitErr)
		}
		if strings.Contains(detail, "remote port forwarding failed") {
			detail += "; remote listening port is occupied or denied by SSH policy; choose another remote port or inspect existing forwards"
		}
		return 1, fmt.Errorf("forward startup failed (%s): %s", runtime.Description, detail)
	}
	if marker != forwardReadyMarker+"\n" {
		return 1, fmt.Errorf("unexpected forward startup response: %q", termui.CleanText(marker))
	}
	*started = true
	if initial {
		if e = runtime.Ready(); e != nil {
			return 1, e
		}
	} else {
		runtime.Status("connected", nil)
	}
	task, err := runtime.Heartbeat()
	if err != nil {
		return 1, err
	}
	heartbeat.Go(func() {
		pulse := func() {
			if err := wire.WriteFull(input, task); err != nil {
				cancel(fmt.Errorf("forward heartbeat write failed: %w", err))
			}
		}
		pulse()
		if ctx.Err() != nil {
			return
		}
		stop, err := startDaemonHeartbeat(ctx, pulse)
		if err != nil {
			cancel(fmt.Errorf("forward heartbeat registration failed: %w", err))
			return
		}
		<-ctx.Done()
		stop()
	})

	select {
	case <-done:
		if parent.Err() != nil {
			return 0, nil
		}
		if cause := context.Cause(ctx); cause != nil {
			return 1, cause
		}
		if waitErr != nil {
			return 1, waitErr
		}
		return 1, fmt.Errorf("forward transport closed")
	case <-ctx.Done():
		if parent.Err() != nil {
			return 0, nil
		}
		return 1, context.Cause(ctx)
	}
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

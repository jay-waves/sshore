package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	forwardio "github.com/jay-waves/sshore/internal/forward"
	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/xtaci/smux"
)

type shellConfig struct {
	NoLogin, NoProfile bool
	Pin                bool
	Local              bool
	Host, Name, Server string
	Command, Instance  string
	OpenIfUnavailable  bool
	ParentKey          string `json:",omitempty"`
	SSH                []string
}

func (o options) shellConfig() shellConfig {
	return shellConfig{
		Command: o.command, Instance: o.instance, Local: o.local, Host: o.host, Name: o.session, Server: o.server, SSH: o.ssh,
		NoLogin: o.noLogin, NoProfile: o.noProfile, Pin: o.pin, OpenIfUnavailable: o.openIfUnavailable, ParentKey: o.remoteParentKey,
	}
}

func openRemoteShellTransport(ctx context.Context, o *options, transports clientTransports, auth *sshAuthEndpoint) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cwd := o.sshDirectory
	var err error
	if cwd == "" {
		cwd, err = os.Getwd()
	}
	if err != nil {
		return nil, err
	}
	if auth == nil && (o.parent == "" || o.command == "sh" || o.openIfUnavailable) {
		var stopAuth func()
		auth, stopAuth, err = startSSHAuthBroker()
		if err != nil {
			return nil, err
		}
		defer stopAuth()
	}
	env := o.environment
	if env == nil {
		env = os.Environ()
	}
	config := o.shellConfig()
	request := localDaemonRequest{Kind: "remote-shell", Record: localShellRecord{Name: "remote-shell", Directory: cwd}, Env: env, Auth: auth, Parent: o.parent, Shell: &config}
	c, err := openLocalDaemonContext(ctx, request)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	mx, err := smux.Client(c, relayMuxConfig())
	if err != nil {
		c.Close()
		return nil, err
	}
	setup, err := mx.OpenStream()
	if err == nil {
		chooseShell := o.command == "attach" && o.session == "" && o.instance == ""
		var ready message
		ready, err = negotiateShellSetup(setup, o.host, chooseShell, o.openIfUnavailable)
		setup.Close()
		if err == nil && ready.Reason != "" {
			o.command, o.session, o.instance = "sh", "", ready.Instance
			o.taskIdentity = ""
		}
	}
	if err != nil {
		mx.Close()
		return nil, err
	}
	open := func(ctx context.Context) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return mx.OpenStream()
	}
	// From here the attachment owns cancellation; the mux owns its connection.
	stop()
	return transports.openTaskStream(ctx, o, open, func() { mx.Close() })
}

type shellDiagnostics struct {
	mu     sync.Mutex
	c      net.Conn
	active bool
}

func (d *shellDiagnostics) Write(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active {
		for offset := 0; offset < len(b); {
			end := min(offset+4096, len(b))
			if err := writeMessage(d.c, message{Type: "setup_diagnostic", Error: string(b[offset:end])}); err != nil {
				return offset, err
			}
			offset = end
		}
	}
	return len(b), nil
}

// Observe frontend EOF during setup as well as during stream relaying.
type setupConnection struct {
	net.Conn
	disconnected context.CancelFunc
}

func (c setupConnection) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.disconnected()
	}
	return n, err
}

func runRemoteShell(c net.Conn, req localDaemonRequest, tasks *daemonTasks) error {
	f := req.Shell
	if f == nil || f.Command != "sh" && f.Command != "attach" {
		return fmt.Errorf("invalid remote shell request")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := c
	var owner *shellTask
	if req.Parent == "" {
		conn = setupConnection{c, cancel}
	} else {
		parent, ok := tasks.lookup(req.Parent).(*nativeShellTask)
		if ok {
			owner = &parent.shell.shellTask
		}
	}
	frontend, err := smux.Server(conn, relayMuxConfig())
	if err != nil {
		return err
	}
	defer frontend.Close()
	setup, err := frontend.AcceptStream()
	if err != nil {
		return err
	}
	defer setup.Close()
	if req.Parent != "" {
		if owner == nil || !owner.suspendedBy(f.ParentKey) {
			return writeMessage(setup, message{Type: "fatal", Error: "parent shell attachment changed"})
		}
		go func() {
			select {
			case <-owner.done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return withRemoteShell(ctx, setup, req, owner, func(open taskEndpoint, _ resourceEndpoint, ready message, ownerDone <-chan struct{}) error {
		if err := streamMessage(setup, ready); err != nil {
			return err
		}
		setup.Close()
		stop := context.AfterFunc(ctx, func() { frontend.Close() })
		defer stop()
		if ownerDone != nil {
			go func() {
				select {
				case <-ownerDone:
					frontend.Close()
				case <-ctx.Done():
				}
			}()
		}
		relayRemoteStreams(frontend, open)
		if owner != nil && f.Command == "sh" {
			<-ownerDone
		}
		return nil
	})
}

func relayRemoteStreams(frontend *smux.Session, open taskEndpoint) {
	var streams sync.WaitGroup
	defer streams.Wait()
	defer frontend.Close()
	for {
		stream, err := frontend.AcceptStream()
		if err != nil {
			return
		}
		streams.Go(func() {
			target, err := open()
			if err != nil {
				stream.Close()
				return
			}
			forwardio.Bridge(stream, target)
		})
	}
}

type taskEndpoint func() (net.Conn, error)

type resourceEndpoint func(context.Context) (net.Conn, error)

type resourceSSHStream struct {
	net.Conn
	transport *transportio.Client
}

func (s resourceSSHStream) Close() error { return s.transport.Close() }

type remoteShellChild struct {
	host     string
	uid      string
	open     taskEndpoint
	resource resourceEndpoint
	done     <-chan struct{}
}

func (s *shellTask) rememberRemoteChild(child *remoteShellChild) func() {
	s.mu.Lock()
	if s.remoteChildren == nil {
		s.remoteChildren = make(map[*remoteShellChild]struct{})
	}
	s.remoteChildren[child] = struct{}{}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.remoteChildren, child)
		s.mu.Unlock()
	}
}

func (child *remoteShellChild) status() (message, error) {
	select {
	case <-child.done:
		return message{}, fmt.Errorf("child shell route ended")
	default:
	}
	c, err := child.open()
	if err != nil {
		return message{}, err
	}
	defer c.Close()
	timeout := time.AfterFunc(5*time.Second, func() { c.Close() })
	defer timeout.Stop()
	reply, err := exchangeRouteRequest(c, message{Type: "status", Action: "health"})
	if err == nil && (reply.Type != "session" || reply.Instance != child.uid || reply.SoftwareVersion != version) {
		err = fmt.Errorf("child shell instance changed")
	}
	return reply, err
}

func ownedTaskEndpoint(open taskEndpoint, uid, token string) taskEndpoint {
	return func() (net.Conn, error) {
		c, err := open()
		if err != nil {
			return nil, err
		}
		timeout := time.AfterFunc(5*time.Second, func() { c.Close() })
		defer timeout.Stop()
		reply, err := exchangeRouteRequest(c, message{Type: "shell_owner_access", Instance: uid, Token: token})
		if err == nil && (reply.Type != "shell_owner_access_ready" || reply.SoftwareVersion != version) {
			err = fmt.Errorf("child shell route unavailable")
		}
		if err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
}

func claimShellOwner(ctx context.Context, open taskEndpoint, instance, owner, token string, hosted bool) (func(), <-chan struct{}, error) {
	c, err := open()
	if err != nil {
		return nil, nil, err
	}
	handshake := time.AfterFunc(5*time.Second, func() { c.Close() })
	defer handshake.Stop()
	if err := writeMessage(c, message{Type: "shell_owner", Instance: instance, Owner: owner, Token: token, SoftwareVersion: version}); err != nil {
		c.Close()
		return nil, nil, err
	}
	return acceptActivityLease(ctx, c, hosted)
}

func withRemoteShell(parent context.Context, c net.Conn, req localDaemonRequest, owner *shellTask, use func(taskEndpoint, resourceEndpoint, message, <-chan struct{}) error) (resultErr error) {
	f := req.Shell
	diagnostics := &shellDiagnostics{c: c, active: true}
	var pipes *transportio.Process
	defer func() {
		if resultErr != nil {
			diagnostics.mu.Lock()
			diagnostics.active = false
			_ = writeMessage(c, message{Type: "fatal", Error: resultErr.Error()})
			diagnostics.mu.Unlock()
		}
		c.Close()
		if pipes != nil {
			_ = pipes.CloseAndWait()
		}
	}()
	if err := validSSHDestination(f.Host); err != nil {
		return err
	}
	if owner != nil && f.Command == "attach" {
		child, err := owner.selectRemoteChild(f)
		if err == nil {
			ready := message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: child.uid}
			return use(child.open, child.resource, ready, child.done)
		}
		if !f.OpenIfUnavailable {
			return err
		}
	}
	shellRequest := remoteServiceRequest{Operation: "shell", Mode: "new", Name: f.Name, Instance: f.Instance}
	if f.Command == "attach" {
		shellRequest.Mode = "attach"
		shellRequest.OpenIfUnavailable = f.OpenIfUnavailable
		if validTaskUID(f.Instance) {
			shellRequest.Name = f.Instance
		}
	}
	connection := remoteService(sshConnectionOptions(options{host: f.Host, ssh: f.SSH, sshDirectory: req.Record.Directory}), f.Server, shellRequest)
	connection.Environment = req.Env
	connection.Confirm, connection.Secret = req.Auth.callbacks()
	connection = withSSHDiagnostics(connection, diagnostics, req.Auth)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var err error
	// Keep the exec channel when an older service closes stdin immediately:
	// its stdout, stderr and exit status can still explain the failure.
	writeRequest := connection.WriteRequest
	connection.WriteRequest = nil
	pipes, err = transportio.Start(ctx, connection, diagnostics)
	if err != nil {
		return err
	}
	since := time.Now()
	if err := writeRequest(pipes); err != nil && err != io.EOF {
		return err
	}
	ready, err := diagnostics.relaySetup(pipes, f.Server)
	if err != nil {
		return err
	}
	record, err := remoteTaskRecord(ready)
	if err != nil {
		return err
	}
	// After setup_ready only the optional inventory remains on exec stdout.
	go func() {
		var shells []shellView
		if err := json.UnmarshalRead(io.LimitReader(pipes, daemonBodyLimit), &shells); err == nil && shells != nil {
			_ = reconcileParticipatingShells(f.Host, shells, since)
		}
	}()
	ready.LocalRequest = nil
	open := func() (net.Conn, error) {
		authCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return openRemoteTaskEndpoint(authCtx, record, pipes.DialTCP)
	}
	// A nested resource redials here, where the original SSH options and
	// credentials are available. The shell transport and its broker may end.
	resourceSSH := connection
	resourceSSH.Args = transportio.WithoutForwarding(connection.Args)
	resourceSSH.Confirm, resourceSSH.Secret = nil, nil
	resourceSSH.RequireKnownHost = true
	resourceSSH = withSSHDiagnostics(resourceSSH, io.Discard, nil)
	resource := func(ctx context.Context) (net.Conn, error) {
		client, err := transportio.Dial(ctx, resourceSSH)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { _ = client.Close() })
		defer stop()
		stream, err := openRemoteTaskEndpoint(ctx, record, client.DialTCP)
		if err != nil {
			client.Close()
			return nil, err
		}
		return resourceSSHStream{Conn: stream, transport: client}, nil
	}
	var ownerDone <-chan struct{}
	if f.Command == "sh" || ready.Reason != "" {
		var uid, token string
		child := owner != nil && f.Command == "sh"
		if child {
			uid, token = owner.record.UID, resourceToken()
		}
		stop, done, err := claimShellOwner(ctx, open, ready.Instance, uid, token, true)
		if err != nil {
			return err
		}
		defer stop()
		ownerDone = done
		if child {
			open = ownedTaskEndpoint(open, ready.Instance, token)
			forget := owner.rememberRemoteChild(&remoteShellChild{host: f.Host, uid: ready.Instance, open: open, resource: resource, done: done})
			defer forget()
		}
	}
	return use(open, resource, ready, ownerDone)
}

func remoteTaskRecord(ready message) (localShellRecord, error) {
	if ready.LocalRequest == nil {
		return localShellRecord{}, fmt.Errorf("remote daemon endpoint missing")
	}
	record := ready.LocalRequest.Record
	host, _, err := net.SplitHostPort(record.Address)
	if err != nil || host != "127.0.0.1" || !validTaskUID(record.UID) || ready.Instance != record.UID || len(record.Token) != 64 {
		return localShellRecord{}, fmt.Errorf("invalid daemon endpoint")
	}
	return record, checkPeerVersion(record.Version, "daemon")
}

// Relay the selection exchange until the remote setup process hands over a task.
func (d *shellDiagnostics) relaySetup(pipes *transportio.Process, server string) (message, error) {
	for {
		m, err := readMessage(pipes)
		if err != nil {
			if err == io.EOF {
				return message{}, remoteSetupExit(pipes, server)
			}
			return message{}, fmt.Errorf("remote shell setup failed: %w", err)
		}
		if err := m.peerError("remote setup"); err != nil {
			return message{}, err
		}
		switch m.Type {
		case "setup_ready":
			if m.Action != "managed" {
				return message{}, fmt.Errorf("invalid remote shell setup mode")
			}
			// Drain diagnostics before the frontend advances to the task protocol.
			d.mu.Lock()
			d.active = false
			d.mu.Unlock()
			return m, nil
		case "session_choice":
			d.mu.Lock()
			err := writeMessage(d.c, m)
			d.mu.Unlock()
			if err != nil {
				return message{}, err
			}
			answer, err := readMessage(d.c)
			if err != nil {
				return message{}, err
			}
			if err := writeMessage(pipes, answer); err != nil {
				return message{}, err
			}
		default:
			return message{}, fmt.Errorf("unexpected remote shell setup response")
		}
	}
}

func remoteSetupExit(pipes *transportio.Process, server string) error {
	// EOF can precede stderr and exit-status. Bound the remaining diagnostics
	// on this failed connection, including any sibling channel open.
	timer := time.AfterFunc(3*time.Second, func() { pipes.Close() })
	defer timer.Stop()
	_ = pipes.CloseWrite()
	exit := pipes.Wait()
	cause := fmt.Errorf("remote shell setup ended before the handshake completed: %w", io.EOF)
	if exit != nil {
		cause = fmt.Errorf("remote shell setup: %w", exit)
		status, ok := errors.AsType[exitStatusError](exit)
		if !ok || status.ExitStatus() != 2 {
			return cause
		}
	}
	// Legacy services reject a different request format as a usage error.
	probe, err := pipes.OpenSibling(transportio.Options{Command: remoteExecutable(server) + " --version"}, io.Discard)
	if err != nil {
		return cause
	}
	defer probe.CloseAndWait()
	_ = probe.CloseWrite()
	const maxVersion = 256
	out, err := io.ReadAll(io.LimitReader(probe, maxVersion+1))
	if err == nil && len(out) <= maxVersion && probe.Wait() == nil {
		if peer, err := parseExecutableVersion(out); err == nil && peer != version {
			return checkPeerVersion(peer, "remote executable")
		}
	}
	return cause
}

// The local frontend-to-daemon connection carries independent task endpoints.
func relayMuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	// This mux runs only between the local frontend and daemon. Larger frames
	// reduce per-frame overhead for bulk streams without changing WAN relays.
	c.MaxFrameSize = 32 * 1024
	c.MaxStreamBuffer = 1024 * 1024
	c.MaxReceiveBuffer = 44 * c.MaxStreamBuffer
	c.KeepAliveInterval = 10 * time.Second
	c.KeepAliveTimeout = 60 * time.Second
	return c
}

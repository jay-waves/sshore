package main

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/xtaci/smux"
)

type shellConfig struct {
	NoLogin, NoProfile bool
	Pin                bool
	Local              bool
	Host, Name         string
	StartupDirectory   string
	Command, Instance  string
	ParentKey          string `json:",omitempty"`
	SSH                []string
	Argv               []string
}

func (o options) shellConfig() shellConfig {
	return shellConfig{
		StartupDirectory: o.startupDirectory, Command: o.command, Instance: o.instance, Local: o.local, Host: o.host, Name: o.session, SSH: o.ssh,
		NoLogin: o.noLogin, NoProfile: o.noProfile, Pin: o.pin, Argv: o.argv, ParentKey: o.remoteParentKey,
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
	if auth == nil && (o.parent == "" || o.command == "sh") {
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
		ready, err = negotiateShellSetup(setup, o.host, chooseShell)
		o.hostInfo = ready.advertisedHost()
		setup.Close()
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
	var owner *tmuxTask
	if req.Parent == "" {
		conn = setupConnection{c, cancel}
	} else {
		parent, ok := tasks.lookup(req.Parent).(*localShell)
		if ok {
			owner = &parent.tmuxTask
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
		if owner == nil || !owner.runtime.Suspended(f.ParentKey) {
			return writeMessage(setup, message{Type: "fatal", Error: "parent shell attachment changed"})
		}
		go func() {
			select {
			case <-owner.runtime.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return withRemoteShell(ctx, setup, req, owner, func(target *shellTarget) error {
		if err := streamMessage(setup, target.ready()); err != nil {
			return err
		}
		setup.Close()
		stop := context.AfterFunc(ctx, func() { frontend.Close() })
		defer stop()
		if target.done != nil {
			go func() {
				select {
				case <-target.done:
					frontend.Close()
				case <-ctx.Done():
				}
			}()
		}
		relayRemoteStreams(frontend, target.open)
		if owner != nil && f.Command == "sh" {
			<-target.done
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
			relayRoute(stream, target)
		})
	}
}

type taskEndpoint func() (net.Conn, error)

type resourceSSHStream struct {
	net.Conn
	transport *transportio.Client
}

func (s resourceSSHStream) Close() error { return s.transport.Close() }

// shellTarget describes one authenticated task. Resources dial separately so
// they can outlive the shell transport.
type shellTarget struct {
	hostInfo hostInfo
	host     string
	uid      string
	name     string
	open     taskEndpoint
	resource func(context.Context) (net.Conn, error)
	done     <-chan struct{}
}

func (target *shellTarget) ready() message {
	return message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: target.uid, Host: &hostAnnouncement{Info: target.hostInfo}}
}

func (s *tmuxTask) rememberRemoteChild(child *shellTarget) func() {
	s.mu.Lock()
	if s.remoteChildren == nil {
		s.remoteChildren = make(map[*shellTarget]struct{})
	}
	s.remoteChildren[child] = struct{}{}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.remoteChildren, child)
		s.mu.Unlock()
	}
}

func (child *shellTarget) status() (message, error) {
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
	reply, err := exchangeRouteRequest(c, message{Type: "status"})
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

func withRemoteShell(parent context.Context, c net.Conn, req localDaemonRequest, owner *tmuxTask, use func(*shellTarget) error) (resultErr error) {
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
		if err != nil {
			return err
		}
		return use(child)
	}
	shellRequest := remoteServiceRequest{Operation: "shell", Mode: "new", Name: f.Name, Instance: f.Instance, Directory: f.StartupDirectory}
	if f.Command == "attach" {
		shellRequest.Mode = "attach"
		if validTaskUID(f.Instance) {
			shellRequest.Name = f.Instance
		}
	}
	connection := remoteService(sshConnectionOptions(options{host: f.Host, ssh: f.SSH, sshDirectory: req.Record.Directory}), shellRequest)
	connection.Environment = req.Env
	connection.Confirm, connection.Secret = req.Auth.callbacks()
	connection = withSSHDiagnostics(connection, diagnostics, req.Auth)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var err error
	pipes, err = transportio.Start(ctx, connection, diagnostics)
	if err != nil {
		return err
	}
	since := time.Now()
	ready, err := diagnostics.relaySetup(pipes)
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
	destination := pipes.Destination()
	info := ready.advertisedHost()
	var serverIP string
	if ready.Host != nil {
		serverIP = ready.Host.SSHServer
	}
	info.User, info.Address, info.Port = destination.User, destination.ObservedAddress(serverIP), destination.Port
	target := &shellTarget{host: f.Host, uid: record.UID, name: record.Name, hostInfo: info}
	target.open = func() (net.Conn, error) {
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
	target.resource = func(ctx context.Context) (net.Conn, error) {
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
	if f.Command == "sh" {
		var uid, token string
		if owner != nil {
			uid, token = owner.record.UID, resourceToken()
		}
		stop, done, err := claimShellOwner(ctx, target.open, target.uid, uid, token, true)
		if err != nil {
			return err
		}
		defer stop()
		target.done = done
		if owner != nil {
			target.open = ownedTaskEndpoint(target.open, target.uid, token)
			forget := owner.rememberRemoteChild(target)
			defer forget()
		}
	}
	return use(target)
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
func (d *shellDiagnostics) relaySetup(pipes *transportio.Process) (message, error) {
	for {
		m, err := readMessage(pipes)
		if err != nil {
			if err == io.EOF {
				return message{}, remoteSetupExit(pipes)
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

func remoteSetupExit(pipes *transportio.Process) error {
	// EOF can precede stderr and exit-status. Bound the remaining diagnostics
	// on this failed connection.
	timer := time.AfterFunc(3*time.Second, func() { pipes.Close() })
	defer timer.Stop()
	_ = pipes.CloseWrite()
	if err := pipes.Wait(); err != nil {
		return fmt.Errorf("remote shell setup: %w", err)
	}
	return fmt.Errorf("remote shell setup ended before the handshake completed: %w", io.EOF)
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

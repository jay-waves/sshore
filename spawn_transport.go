package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	forwardio "github.com/jay-waves/sshore/internal/forward"
	"github.com/jay-waves/sshore/internal/proc"
	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/xtaci/smux"
)

type spawnConfig struct {
	NoLogin, NoProfile bool
	Tree               []shellTreeNode
	Local              bool
	Host, Name, Server string
	Command, Instance  string
	SSH                []string
}

func openRemoteShellTransport(ctx context.Context, o *options, transports clientTransports) (net.Conn, error) {
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
	auth, stopAuth, err := startSSHAuthBroker()
	if err != nil {
		return nil, err
	}
	defer stopAuth()
	env := o.environment
	if env == nil {
		env = os.Environ()
	}
	request := localDaemonRequest{Kind: "remote-shell", Record: localShellRecord{Name: "remote-shell", Directory: cwd, Owner: o.owner}, Env: env, Auth: auth, Spawn: &spawnConfig{Host: o.host, Name: o.session, Server: o.server, SSH: o.ssh, Command: o.command, Instance: o.instance}}
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
		_, err = negotiateSetup(setup, setup, o.host, chooseShell)
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

type spawnDiagnostics struct {
	mu     sync.Mutex
	c      net.Conn
	active bool
}

func (d *spawnDiagnostics) Write(b []byte) (int, error) {
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

func runRemoteShell(c net.Conn, req localDaemonRequest) error {
	f := req.Spawn
	if f == nil || f.Command != "launch" && f.Command != "attach" && f.Command != "spawn" {
		return fmt.Errorf("invalid remote shell request")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := c
	if f.Command != "spawn" {
		conn = setupConnection{c, cancel}
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
	if f.Command == "spawn" {
		return withRemoteSpawn(setup, req, func(open taskEndpoint, ready message, ownerDone <-chan struct{}) error {
			if err := streamMessage(setup, ready); err != nil {
				return err
			}
			setup.Close()
			relayDirectSpawn(frontend, ownerDone, open)
			return nil
		})
	}

	return withRemoteShell(ctx, setup, req, f.Command, func(open taskEndpoint, ready message) error {
		if err := streamMessage(setup, ready); err != nil {
			return err
		}
		setup.Close()
		relayRemoteStreams(frontend, open)
		return nil
	})
}

// The owner channel, not an attachment, holds the temporary shell alive.
// Data endpoints are sibling SSH channels opened by the authenticated dialer.
func relayDirectSpawn(frontend *smux.Session, ownerDone <-chan struct{}, open taskEndpoint) {
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		relayRemoteStreams(frontend, whileSpawnOwned(ownerDone, open))
	}()

	// Detach/front-end EOF does not revoke the owner's lease.
	<-ownerDone
	// Owner EOF can precede delivery of bytes already queued on sibling channels.
	// Reject new endpoints immediately, but let the existing output drain briefly.
	timer := time.NewTimer(spawnDrainTimeout)
	defer timer.Stop()
	select {
	case <-relayDone:
	case <-timer.C:
	}
	frontend.Close()
	<-relayDone
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

// Keep PID and creation identity together when a process outlives its launcher.
type processIdentity struct {
	PID   int
	Start string
}

func currentSpawnOwner() (processIdentity, error) {
	pid := os.Getppid()
	if pid <= 1 {
		return processIdentity{}, fmt.Errorf("spawn requires a live parent shell")
	}
	start, err := proc.LiveIdentity(pid)
	return processIdentity{PID: pid, Start: start}, err
}

// The task owns this watcher. Stopping it joins any in-flight owner-loss
// callback, so cleanup cannot affect a later attachment or outlive the task.
func superviseSpawnOwner(owner processIdentity, lost func()) func() {
	if owner.PID == 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		waitSpawnOwner(ctx, owner)
		if ctx.Err() == nil {
			lost()
		}
	}()
	return func() { cancel(); <-stopped }
}

func waitSpawnOwner(ctx context.Context, owner processIdentity) {
	request := activityRequest{Kind: "owner", Owner: owner}
	if s := accountActivity.Load(); s != nil {
		lease, err := s.registerLease(request)
		if err != nil {
			return
		}
		defer s.remove(lease)
		select {
		case <-ctx.Done():
		case <-lease.expired:
		}
		return
	}
	stop, done, err := remoteActivity(ctx, request)
	if err != nil {
		return
	}
	defer stop()
	<-done
}

const spawnDrainTimeout = 2 * time.Second

type taskEndpoint func() (net.Conn, error)

func spawnEnded(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func whileSpawnOwned(done <-chan struct{}, open taskEndpoint) taskEndpoint {
	return func() (net.Conn, error) {
		if spawnEnded(done) {
			return nil, io.ErrClosedPipe
		}
		c, err := open()
		if err == nil && spawnEnded(done) {
			c.Close()
			return nil, io.ErrClosedPipe
		}
		return c, err
	}
}

func claimSpawnOwner(ctx context.Context, open taskEndpoint, instance string) (func(), <-chan struct{}, error) {
	owner, err := open()
	if err != nil {
		return nil, nil, err
	}
	handshake := time.AfterFunc(5*time.Second, func() { owner.Close() })
	defer handshake.Stop()
	if err := writeMessage(owner, message{Type: "spawn_owner", SoftwareVersion: version, Instance: instance}); err != nil {
		owner.Close()
		return nil, nil, err
	}
	return acceptActivityLease(ctx, owner)
}

func withRemoteSpawn(c net.Conn, req localDaemonRequest, use func(taskEndpoint, message, <-chan struct{}) error) error {
	return withRemoteShell(context.Background(), c, req, "spawn", func(open taskEndpoint, ready message) error {
		stop, done, err := claimSpawnOwner(context.Background(), open, ready.Instance)
		if err != nil {
			return err
		}
		defer func() {
			stop()
			if err := confirmKilledShell(req.Spawn.Host, "", ready.Instance); err != nil {
				fmt.Fprintf(os.Stderr, "sshore: record spawn lifetime end: %v\n", err)
			}
		}()
		return use(open, ready, done)
	})
}

func withRemoteShell(parent context.Context, c net.Conn, req localDaemonRequest, command string, use func(taskEndpoint, message) error) (resultErr error) {
	f := req.Spawn
	diagnostics := &spawnDiagnostics{c: c, active: true}
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
	if f == nil || command == "spawn" && req.Record.Owner.PID <= 1 {
		return fmt.Errorf("invalid remote shell request")
	}
	if err := validSSHDestination(f.Host); err != nil {
		return err
	}
	o := options{command: command, host: f.Host, session: f.Name, instance: f.Instance, server: f.Server, ssh: f.SSH, sshDirectory: req.Record.Directory}
	connection := sshConnectionOptions(o)
	connection.Environment = req.Env
	connection.Confirm, connection.Secret = req.Auth.callbacks()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if command == "spawn" {
		stopWatching := superviseSpawnOwner(req.Record.Owner, func() { cancel(); c.Close() })
		defer stopWatching()
	}
	var err error
	pipes, err = transportio.Start(ctx, connection, diagnostics)
	if err != nil {
		return err
	}
	ready, err := relayShellSetup(c, pipes, diagnostics)
	if err != nil {
		return err
	}
	if ready.LocalRequest == nil {
		return fmt.Errorf("remote daemon endpoint missing")
	}
	record := ready.LocalRequest.Record
	host, _, err := net.SplitHostPort(record.Address)
	if err != nil || host != "127.0.0.1" || !validTaskUID(record.UID) || ready.Instance != record.UID || len(record.Token) != 64 {
		return fmt.Errorf("invalid daemon endpoint")
	}
	if err := checkPeerVersion(record.Version, "daemon"); err != nil {
		return err
	}
	ready.LocalRequest = nil
	open := func() (net.Conn, error) {
		authCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		channel, err := pipes.DialTCP(authCtx, record.Address)
		if err != nil {
			return nil, err
		}
		return upgradeDaemonConn(authCtx, channel, record, "/tasks/"+record.UID+"/open", nil)
	}
	return use(open, ready)
}

// Relay the selection exchange until the remote setup process hands over a task.
func relayShellSetup(setup net.Conn, pipes io.ReadWriter, diagnostics *spawnDiagnostics) (message, error) {
	for {
		m, err := readMessage(pipes)
		if err != nil {
			return message{}, fmt.Errorf("remote shell setup failed: %w", err)
		}
		switch m.Type {
		case "fatal":
			return message{}, fmt.Errorf("%s", m.Error)
		case "setup_ready":
			if m.Action != "managed" {
				return message{}, fmt.Errorf("invalid remote shell setup mode")
			}
			if err := checkPeerVersion(m.SoftwareVersion, "remote setup"); err != nil {
				return message{}, err
			}
			// Drain diagnostics before the frontend advances to the task protocol.
			diagnostics.mu.Lock()
			diagnostics.active = false
			diagnostics.mu.Unlock()
			return m, nil
		case "session_choice":
			diagnostics.mu.Lock()
			err := writeMessage(setup, m)
			diagnostics.mu.Unlock()
			if err != nil {
				return message{}, err
			}
			answer, err := readMessage(setup)
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

func shellTaskName(name string, temporary bool) string {
	if temporary {
		return "spawn/" + name
	}
	return "launch/" + name
}

// Lifetime is fixed by startup; attachment state never promotes a spawn.
func shellLifetime(temporary, remote bool) string {
	if !temporary {
		return "persistent"
	}
	if remote {
		return "heartbeat"
	}
	return "parent"
}

func validShellLifetime(value string) bool {
	return value == "" || value == "persistent" || value == "parent" || value == "heartbeat"
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

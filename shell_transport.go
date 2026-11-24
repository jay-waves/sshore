package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
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
	ParentKey          string `json:",omitempty"`
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
	var auth *sshAuthEndpoint
	if o.parent == "" || o.command == "sh" {
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
	request := localDaemonRequest{Kind: "remote-shell", Record: localShellRecord{Name: "remote-shell", Directory: cwd}, Env: env, Auth: auth, Parent: o.parent, Shell: &shellConfig{Host: o.host, Name: o.session, Server: o.server, SSH: o.ssh, Command: o.command, Instance: o.instance, ParentKey: o.remoteParentKey}}
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
	return withRemoteShell(ctx, setup, req, owner, func(open taskEndpoint, ready message, ownerDone <-chan struct{}) error {
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

// Keep PID and creation identity together when a process outlives its launcher.
type processIdentity struct {
	PID   int
	Start string
}

type taskEndpoint func() (net.Conn, error)

type remoteShellChild struct {
	host string
	uid  string
	open taskEndpoint
	done <-chan struct{}
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

func (s *shellTask) remoteChildIDs(prefix string) []selectedTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []selectedTask
	for child := range s.remoteChildren {
		if strings.HasPrefix(child.uid, prefix) {
			candidates = append(candidates, selectedTask{Kind: "shell", Identity: identityFromUID(child.uid), Instance: child.uid, ChildHost: child.host})
		}
	}
	return candidates
}

func (s *shellTask) selectRemoteChild(config *shellConfig) (*remoteShellChild, error) {
	if config.Name == "" && config.Instance == "" {
		return nil, fmt.Errorf("select a child with sshore attach HOST:ID")
	}
	s.mu.Lock()
	var children []*remoteShellChild
	for child := range s.remoteChildren {
		if child.host == config.Host && (config.Instance == "" || child.uid == config.Instance) {
			children = append(children, child)
		}
	}
	s.mu.Unlock()

	// Status and ownership come from the child's authenticated endpoint.
	type liveChild struct {
		child *remoteShellChild
		state string
	}
	live := make(map[string]liveChild)
	var candidates []selectedTask
	var lookupErr error
	for _, child := range children {
		status, err := child.status()
		if err != nil {
			lookupErr = fmt.Errorf("could not check child %s on %s: %w", child.uid[:6], cleanText(child.host), err)
			continue
		}
		if status.Owner == s.treeIdentity() {
			id := identityFromUID(child.uid)
			live[id] = liveChild{child, status.Action}
			candidates = append(candidates, selectedTask{Identity: id, Name: status.Name})
		}
	}
	ref := config.Name
	if config.Instance != "" {
		ref = config.Instance
	}
	target, err := matchTask(ref, candidates)
	if err != nil {
		return nil, err
	}
	match, ok := live[target.Identity]
	if !ok {
		if lookupErr != nil {
			return nil, lookupErr
		}
		return nil, fmt.Errorf("no live child matches %s on %s; enter it from the shell that opened it", cleanText(ref), cleanText(config.Host))
	}
	if match.state != taskDetached {
		return nil, fmt.Errorf("child shell %s is %s; detach it before attaching", taskShortCode(target.Identity), cleanText(match.state))
	}
	return match.child, nil
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

func withRemoteShell(parent context.Context, c net.Conn, req localDaemonRequest, owner *shellTask, use func(taskEndpoint, message, <-chan struct{}) error) (resultErr error) {
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
	if f == nil {
		return fmt.Errorf("invalid remote shell request")
	}
	if err := validSSHDestination(f.Host); err != nil {
		return err
	}
	if owner != nil && f.Command == "attach" {
		child, err := owner.selectRemoteChild(f)
		if err != nil {
			return err
		}
		ready := message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: child.uid}
		return use(child.open, ready, child.done)
	}
	o := options{command: f.Command, host: f.Host, session: f.Name, instance: f.Instance, server: f.Server, ssh: f.SSH, sshDirectory: req.Record.Directory}
	connection := sshConnectionOptions(o)
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
	var ownerDone <-chan struct{}
	if f.Command == "sh" {
		var uid, token string
		if owner != nil {
			uid, token = owner.record.UID, resourceToken()
		}
		stop, done, err := claimShellOwner(ctx, open, ready.Instance, uid, token, true)
		if err != nil {
			return err
		}
		defer stop()
		ownerDone = done
		if owner != nil {
			open = ownedTaskEndpoint(open, ready.Instance, token)
			forget := owner.rememberRemoteChild(&remoteShellChild{host: f.Host, uid: ready.Instance, open: open, done: done})
			defer forget()
		}
	}
	since := time.Now()
	if shells, err := connectedHostedShells(ctx, record, pipes.DialTCP); err == nil {
		_ = reconcileParticipatingShells(f.Host, shells, since)
	}
	return use(open, ready, ownerDone)
}

// Query only registered hosted shells using a channel on the authenticated SSH
// connection. The attachment control stream is independent of the query.
func connectedHostedShells(parent context.Context, record localShellRecord, dial func(context.Context, string) (net.Conn, error)) ([]shellView, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	req, err := daemonRequest(ctx, record, "/tasks/snapshot", taskInventoryRequest{Hosted: true, Detail: "health"})
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return dial(ctx, address)
		},
	}
	client := &http.Client{Transport: transport, CheckRedirect: daemonHTTPClient.CheckRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, daemonResponseError(resp)
	}
	var snapshot struct{ Shells json.RawMessage }
	if err := json.NewDecoder(io.LimitReader(resp.Body, daemonBodyLimit)).Decode(&snapshot); err != nil {
		return nil, err
	}
	var shells []shellView
	if err := json.Unmarshal(snapshot.Shells, &shells); err != nil {
		return nil, err
	}
	return shells, nil
}

// Relay the selection exchange until the remote setup process hands over a task.
func relayShellSetup(setup net.Conn, pipes io.ReadWriter, diagnostics *shellDiagnostics) (message, error) {
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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	request := localDaemonRequest{Kind: "remote-shell", Record: localShellRecord{Name: "remote-shell", Directory: cwd}, Env: env, Auth: auth, Parent: o.parent, Shell: &shellConfig{Host: o.host, Name: o.session, Server: o.server, SSH: o.ssh, Command: o.command, Instance: o.instance}}
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
	if req.Parent == "" {
		conn = setupConnection{c, cancel}
	} else {
		parent, ok := tasks.lookup(req.Parent).(*nativeShellTask)
		if !ok || f.Command != "sh" {
			return fmt.Errorf("parent shell is unavailable")
		}
		go func() {
			select {
			case <-parent.shell.done:
				cancel()
			case <-ctx.Done():
			}
		}()
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
	return withRemoteShell(ctx, setup, req, f.Command, func(open taskEndpoint, ready message, ownerDone <-chan struct{}) error {
		if err := streamMessage(setup, ready); err != nil {
			return err
		}
		setup.Close()
		stop := context.AfterFunc(ctx, func() { frontend.Close() })
		defer stop()
		relayRemoteStreams(frontend, open)
		if req.Parent != "" {
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

func claimShellOwner(ctx context.Context, open taskEndpoint, instance string) (func(), <-chan struct{}, error) {
	c, err := open()
	if err != nil {
		return nil, nil, err
	}
	handshake := time.AfterFunc(5*time.Second, func() { c.Close() })
	defer handshake.Stop()
	if err := writeMessage(c, message{Type: "shell_owner", Instance: instance, SoftwareVersion: version}); err != nil {
		c.Close()
		return nil, nil, err
	}
	return acceptActivityLease(ctx, c)
}

func withRemoteShell(parent context.Context, c net.Conn, req localDaemonRequest, command string, use func(taskEndpoint, message, <-chan struct{}) error) (resultErr error) {
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
	o := options{command: command, host: f.Host, session: f.Name, instance: f.Instance, server: f.Server, ssh: f.SSH, sshDirectory: req.Record.Directory}
	connection := sshConnectionOptions(o)
	connection.Environment = req.Env
	connection.Confirm, connection.Secret = req.Auth.callbacks()
	connection.Warn = req.Auth.warn
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
	since := time.Now()
	if shells, err := connectedHostedShells(ctx, record, pipes.DialTCP); err == nil {
		_ = reconcileParticipatingShells(f.Host, shells, since)
	}
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
	if command == "sh" {
		stop, done, err := claimShellOwner(ctx, open, ready.Instance)
		if err != nil {
			return err
		}
		defer stop()
		ownerDone = done
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

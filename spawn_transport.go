package main

import (
	"context"
	"fmt"
	"github.com/jay-waves/sshore/internal/termui"
	"io"
	"net"
	"net/http"
	"os"
	"sync"

	transportio "github.com/jay-waves/sshore/internal/ssh"

	"github.com/jay-waves/sshore/internal/wire"
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

func openRemoteShellTransport(ctx context.Context, o *options) (io.ReadWriteCloser, error) {
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
	auth, stopAuth, err := startSpawnAuth()
	if err != nil {
		return nil, err
	}
	defer stopAuth()
	env := o.environment
	if env == nil {
		env = os.Environ()
	}
	request := localDaemonRequest{Kind: "remote-shell", Record: localShellRecord{Name: "remote-shell", Directory: cwd, Owner: o.owner}, Env: append(append([]string(nil), env...), auth...), Spawn: &spawnConfig{Host: o.host, Name: o.session, Server: o.server, SSH: o.ssh, Command: o.command, Instance: o.instance}}
	c, err := openLocalDaemonContext(ctx, request)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	closeTransport := func() { stop(); c.Close() }
	mx, err := smux.Client(c, wire.MuxConfig())
	if err != nil {
		closeTransport()
		return nil, err
	}
	setup, err := mx.OpenStream()
	if err == nil {
		err = negotiateSetupUsing(ctx, setup, setup, o, setupClientUI{OpenInput: func() (setupInput, error) { return termui.NewInput() }})
		setup.Close()
	}
	if err != nil {
		mx.Close()
		closeTransport()
		return nil, err
	}
	open := func(ctx context.Context) (io.ReadWriteCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return mx.OpenStream()
	}
	if o.transports != nil {
		stop()
	}
	return openTaskStream(ctx, o, open, func() { mx.Close(); closeTransport() })
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
	// A spawn remains owner-bound after the frontend disconnects.
	conn := c
	if f.Command != "spawn" {
		conn = setupConnection{c, cancel}
	}
	frontend, err := smux.Server(conn, wire.MuxConfig())
	if err != nil {
		return err
	}
	defer frontend.Close()
	setup, err := frontend.AcceptStream()
	if err != nil {
		return err
	}
	defer setup.Close()
	return prepareRemoteShellConnection(ctx, setup, req, f.Command, func(open func(context.Context) (io.ReadWriteCloser, error), lifetime *smux.Session) error {
		setup.Close()
		if lifetime == nil {
			relayRemoteStreams(ctx, frontend, open)
			return nil
		}
		done := make(chan struct{})
		go func() { defer close(done); relaySpawnStreams(frontend, lifetime) }()
		<-lifetime.CloseChan()
		frontend.Close()
		<-done
		return nil
	})
}

func relaySpawnStreams(frontend, remote *smux.Session) {
	relayRemoteStreams(context.Background(), frontend, func(context.Context) (io.ReadWriteCloser, error) { return remote.OpenStream() })
}
func relayRemoteStreams(ctx context.Context, frontend *smux.Session, open func(context.Context) (io.ReadWriteCloser, error)) {
	var streams sync.WaitGroup
	defer streams.Wait()
	defer frontend.Close()
	for {
		stream, err := frontend.AcceptStream()
		if err != nil {
			return
		}
		streams.Go(func() {
			target, err := open(ctx)
			if err != nil {
				stream.Close()
				return
			}
			wire.Bridge(stream, target)
		})
	}
}

// The owner supervises a single lifetime connection; attachments use its streams.
func runSpawnConnection(c net.Conn, req localDaemonRequest, attached func(*smux.Session) error) error {
	return prepareRemoteShellConnection(context.Background(), c, req, "spawn", func(_ func(context.Context) (io.ReadWriteCloser, error), lifetime *smux.Session) error {
		return attached(lifetime)
	})
}

func prepareRemoteShellConnection(parent context.Context, c net.Conn, req localDaemonRequest, command string, ready func(func(context.Context) (io.ReadWriteCloser, error), *smux.Session) error) error {
	f := req.Spawn
	if f == nil || command == "spawn" && req.Record.Owner.PID <= 1 {
		return fmt.Errorf("invalid remote shell request")
	}
	if err := validSSHDestination(f.Host); err != nil {
		return err
	}
	o := options{command: command, host: f.Host, session: f.Name, instance: f.Instance, server: f.Server, ssh: f.SSH, sshDirectory: req.Record.Directory}
	diagnostics := &spawnDiagnostics{c: c, active: true}
	connection := sshioOptionsForSpawn(o, req)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if command == "spawn" {
		stopWatching := superviseSpawnOwner(req.Record.Owner, func() { cancel(); c.Close() })
		defer stopWatching()
	}
	pipes, err := transportio.Start(ctx, connection, diagnostics)
	if err != nil {
		_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
		return err
	}
	defer func() { c.Close(); _ = pipes.CloseAndWait() }()
	var transport io.ReadWriteCloser = pipes
	var open func(context.Context) (io.ReadWriteCloser, error)
	// Relay only setup choices. Authentication uses the launcher's askpass broker.
	for {
		m, e := readMessage(pipes)
		if e != nil {
			_ = writeMessage(c, message{Type: "fatal", Error: fmt.Sprintf("remote shell setup failed: %v", e)})
			return e
		}
		if m.Type == "setup_ready" {
			// Drain any in-flight diagnostic write before switching protocols.
			diagnostics.mu.Lock()
			diagnostics.active = false
			diagnostics.mu.Unlock()
			if m.Action != "managed" && m.Action != "updated" {
				_ = writeMessage(c, message{Type: "fatal", Error: "invalid remote shell setup mode"})
				return fmt.Errorf("invalid remote shell setup mode")
			}
			if err := checkPeerVersion(m.SoftwareVersion, "remote setup"); err != nil {
				return err
			}
			if m.LocalRequest != nil {
				record := m.LocalRequest.Record
				open = func(ctx context.Context) (io.ReadWriteCloser, error) {
					return openRemoteDaemonTask(ctx, pipes.DialTCP, record)
				}
				if command == "spawn" {
					channel, err := open(ctx)
					if err != nil {
						return err
					}
					defer channel.Close()
					if err := writeMessage(channel, message{Type: "spawn_transport", SoftwareVersion: version}); err != nil {
						return err
					}
					transport = channel
				}
				m.LocalRequest = nil
			} else if command != "spawn" {
				return fmt.Errorf("remote daemon endpoint missing")
			}
			if err := writeMessage(c, m); err != nil {
				return err
			}
			break
		}
		diagnostics.mu.Lock()
		err = writeMessage(c, m)
		diagnostics.mu.Unlock()
		if err != nil {
			return err
		}
		if m.Type == "fatal" {
			return fmt.Errorf("%s", m.Error)
		}
		if m.Type == "setup_download" {
			if err := relaySetupBinary(pipes, c); err != nil {
				return err
			}
			continue
		}
		if m.Type != "session_choice" && m.Type != "setup_choice" {
			return fmt.Errorf("unexpected remote shell setup response")
		}
		answer, e := readMessage(c)
		if e != nil {
			return e
		}
		if e = writeMessage(pipes, answer); e != nil {
			return e
		}
	}
	if command != "spawn" {
		return ready(open, nil)
	}
	lifetime, err := clientMux(transport)
	if err != nil {
		return err
	}
	defer lifetime.Close()
	return ready(nil, lifetime)
}

func startSpawnAuth() ([]string, func(), error) {
	token := resourceToken()
	prompts := make(chan struct{}, 1)
	l, broker, err := resourceListen(token, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/ask" && r.URL.Path != "/unlock" {
			http.NotFound(w, r)
			return
		}
		select {
		case prompts <- struct{}{}:
			defer func() { <-prompts }()
		case <-r.Context().Done():
			return
		}
		serveSSHConfirmation(w, r)
	})
	if err != nil {
		return nil, nil, err
	}
	env := []string{"SSHORE_CONFIRM_ADDR=" + l.Addr().String(), "SSHORE_CONFIRM_TOKEN=" + token}
	return env, func() { broker.Close() }, nil
}

func serveSSHConfirmation(w http.ResponseWriter, r *http.Request) {
	prompt, err := io.ReadAll(io.LimitReader(r.Body, 8193))
	if err != nil || len(prompt) > 8192 {
		http.Error(w, "invalid prompt", 400)
		return
	}
	if r.URL.Path == "/unlock" {
		secret, err := promptSSHSecret(r.Context(), string(prompt))
		if err != nil {
			http.Error(w, "terminal input failed", 409)
			return
		}
		defer clear(secret)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(secret)
		return
	}
	ok, err := confirmSSHHost(r.Context(), string(prompt))
	if err != nil {
		http.Error(w, "terminal input failed", 409)
		return
	}
	if ok {
		io.WriteString(w, "yes")
	} else {
		io.WriteString(w, "no")
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

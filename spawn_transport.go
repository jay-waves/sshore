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
	"time"

	transportio "github.com/jay-waves/sshore/internal/ssh"

	"github.com/jay-waves/sshore/internal/wire"
	"github.com/xtaci/smux"
)

type spawnConfig struct {
	NoLogin, NoProfile bool
	Tree               []shellTreeNode
	Local              bool
	Host, Name, Server string
	SSH                []string
}

func openSpawnTransport(ctx context.Context, o *options) (io.ReadWriteCloser, error) {
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
	request := localDaemonRequest{Kind: "spawn", Record: localShellRecord{Name: "spawn", Directory: cwd, Owner: o.owner}, Env: append(append([]string(nil), env...), auth...), Spawn: &spawnConfig{Host: o.host, Name: o.session, Server: o.server, SSH: o.ssh}}
	c, err := openLocalDaemonContext(ctx, request)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	closeTransport := func() { stop(); c.Close() }
	_ = c.SetDeadline(time.Time{})
	if err = negotiateSetupUsing(ctx, c, c, o, setupClientUI{OpenInput: func() (setupInput, error) { return termui.NewInput() }}); err != nil {
		closeTransport()
		return nil, err
	}
	mx, err := smux.Client(c, wire.MuxConfig())
	if err != nil {
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

func runSpawnRemote(c net.Conn, req localDaemonRequest) error {
	return runSpawnConnection(c, req, func(mx *smux.Session) error {
		frontend, err := smux.Server(c, wire.MuxConfig())
		if err != nil {
			return err
		}
		relayDone := make(chan struct{})
		go func() { defer close(relayDone); relaySpawnStreams(frontend, mx) }()
		// Detaching the frontend must not release the spawn's lifetime connection.
		<-mx.CloseChan()
		frontend.Close()
		<-relayDone
		return nil
	})
}

func relaySpawnStreams(frontend, remote *smux.Session) {
	var streams sync.WaitGroup
	defer streams.Wait()
	for {
		stream, err := frontend.AcceptStream()
		if err != nil {
			return
		}
		target, err := remote.OpenStream()
		if err != nil {
			stream.Close()
			frontend.Close()
			return
		}
		streams.Go(func() { wire.Bridge(stream, target) })
	}
}

// The owner supervises a single lifetime connection; attachments use its streams.
func runSpawnConnection(c net.Conn, req localDaemonRequest, attached func(*smux.Session) error) error {
	f := req.Spawn
	if f == nil || req.Record.Owner.PID <= 1 {
		return fmt.Errorf("invalid spawn request")
	}
	if err := validSSHDestination(f.Host); err != nil {
		return err
	}
	o := options{command: "spawn", host: f.Host, session: f.Name, server: f.Server, ssh: f.SSH, sshDirectory: req.Record.Directory}
	diagnostics := &spawnDiagnostics{c: c, active: true}
	connection := sshioOptionsForSpawn(o, req)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopWatching := superviseSpawnOwner(req.Record.Owner, func() { cancel(); c.Close() })
	defer stopWatching()
	pipes, err := transportio.Start(ctx, connection, diagnostics)
	if err != nil {
		_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
		return err
	}
	defer func() { c.Close(); _ = pipes.CloseAndWait() }()
	var transport io.ReadWriteCloser = pipes
	// Relay only setup choices. Authentication uses the launcher's askpass broker.
	for {
		m, e := readMessage(pipes)
		if e != nil {
			_ = writeMessage(c, message{Type: "fatal", Error: fmt.Sprintf("spawn setup failed: %v", e)})
			return e
		}
		if m.Type == "setup_ready" {
			// Drain any in-flight diagnostic write before switching protocols.
			diagnostics.mu.Lock()
			diagnostics.active = false
			diagnostics.mu.Unlock()
			if m.Action != "managed" && m.Action != "updated" {
				_ = writeMessage(c, message{Type: "fatal", Error: "invalid spawn setup mode"})
				return fmt.Errorf("invalid spawn setup mode")
			}
			if m.LocalRequest != nil {
				channel, err := openRemoteDaemonTask(ctx, pipes.DialTCP, m.LocalRequest.Record)
				if err != nil {
					return err
				}
				defer channel.Close()
				if err := writeMessage(channel, message{Type: "spawn_transport", SoftwareVersion: version}); err != nil {
					return err
				}
				transport = channel
				m.LocalRequest = nil
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
			return fmt.Errorf("unexpected spawn setup response")
		}
		answer, e := readMessage(c)
		if e != nil {
			return e
		}
		if e = writeMessage(pipes, answer); e != nil {
			return e
		}
	}
	mx, err := clientMux(transport)
	if err != nil {
		return err
	}
	defer mx.Close()
	return attached(mx)
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

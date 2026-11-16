package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

type spawnConfig struct {
	Host, Name, Server string
	SSH                []string
}

// Close both directions and join the reader before releasing either endpoint.
func proxySpawn(a, b io.ReadWriteCloser) {
	done := make(chan struct{})
	go func() { _, _ = io.Copy(b, a); b.Close(); a.Close(); close(done) }()
	_, _ = io.Copy(a, b)
	a.Close()
	b.Close()
	<-done
}

func openSpawnTransport(o *options) (io.ReadWriteCloser, func(), error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	auth, stopAuth, err := startSpawnAuth()
	if err != nil {
		return nil, nil, err
	}
	defer stopAuth()
	request := localDaemonRequest{Kind: "spawn", Record: localShellRecord{Name: "spawn", Directory: cwd, Owner: o.owner}, Env: append(os.Environ(), auth...), Spawn: &spawnConfig{Host: o.host, Name: o.session, Server: o.server, SSH: o.ssh}}
	c, err := openLocalDaemon(request)
	if err != nil {
		return nil, nil, err
	}
	_ = c.SetDeadline(time.Time{})
	if err = negotiateSetup(c, c, o); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, func() { c.Close() }, nil
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
	f := req.Spawn
	if f == nil || req.Record.Owner.PID <= 1 {
		return fmt.Errorf("invalid spawn request")
	}
	if err := validSSHDestination(f.Host); err != nil {
		return err
	}
	o := options{command: "spawn", host: f.Host, session: f.Name, server: f.Server, ssh: f.SSH, sshDirectory: req.Record.Directory}
	template := sshCommand(o)
	// Resolve SSH from the launching client's environment, not daemon startup.
	exe := localEnvironmentValue(req.Env, "SSHORE_SSH")
	if exe == "" {
		exe = "ssh"
	}
	cmd := exec.Command(exe, template.Args[1:]...)
	cmd.Dir = req.Record.Directory
	detachResourceWorker(cmd)
	cmd.Env = req.Env
	diagnostics := &spawnDiagnostics{c: c, active: true}
	cmd.Stderr = diagnostics
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return err
	}
	if err = cmd.Start(); err != nil {
		input.Close()
		output.Close()
		_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
		return err
	}
	done := make(chan struct{})
	defer close(done)
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { c.Close(); input.Close(); output.Close(); _ = cmd.Process.Kill() }) }
	defer func() { stop(); _ = cmd.Wait() }()
	go watchSpawnOwner(req.Record.Owner, done, stop)
	transport := &pipeTransport{Reader: output, Writer: input, Closers: []io.Closer{output, input}}
	// Relay only setup choices. Authentication uses the launcher's askpass broker.
	for {
		m, e := readMessage(output)
		if e != nil {
			_ = writeMessage(c, message{Type: "fatal", Error: fmt.Sprintf("spawn setup failed: %v", e)})
			return e
		}
		diagnostics.mu.Lock()
		if m.Type == "setup_ready" {
			diagnostics.active = false
		}
		err = writeMessage(c, m)
		diagnostics.mu.Unlock()
		if err != nil {
			return err
		}
		if m.Type == "fatal" {
			return fmt.Errorf("%s", m.Error)
		}
		if m.Type == "setup_ready" {
			if m.Action == "plain" {
				proxySpawn(c, transport)
				return nil
			}
			break
		}
		if m.Type == "setup_download" {
			if err := relaySetupBinary(input, c); err != nil {
				return err
			}
			continue
		}
		if m.Type != "setup_choice" && m.Type != "session_choice" {
			return fmt.Errorf("unexpected spawn setup response")
		}
		answer, e := readMessage(c)
		if e != nil {
			return e
		}
		if e = writeMessage(input, answer); e != nil {
			return e
		}
	}
	mx, err := clientMux(transport)
	if err != nil {
		return err
	}
	defer mx.Close()
	stream, err := mx.OpenStream()
	if err != nil {
		return err
	}
	// A detached foreground client releases only its stream. Keep the lease alive
	// until the owning shell or remote spawn ends, including during reattachment.
	proxySpawn(c, stream)
	<-mx.CloseChan()
	return nil
}

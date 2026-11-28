package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

type shellConfig struct {
	NoLogin, NoProfile bool
	Pin                bool
	Local              bool
	Host, Name         string
	StartupDirectory   string
	Command, Instance  string
	SSH                []string
	Argv               []string
}

func (o options) shellConfig() shellConfig {
	return shellConfig{
		StartupDirectory: o.startupDirectory, Command: o.command, Instance: o.instance, Local: o.local, Host: o.host, Name: o.session, SSH: o.ssh,
		NoLogin: o.noLogin, NoProfile: o.noProfile, Pin: o.pin, Argv: o.argv,
	}
}

// The foreground client owns the SSH transport across its same-host switches.
func openRemoteShellTransport(ctx context.Context, o *options, transports clientTransports, auth *sshAuthEndpoint) (net.Conn, error) {
	mode := "new"
	name := o.session
	if o.command == "attach" {
		mode = "attach"
		if validTaskUID(o.instance) {
			name = o.instance
		}
	}
	req := remoteServiceRequest{Operation: "shell", Mode: mode, Name: name, Instance: o.instance, Directory: o.startupDirectory}
	connection := remoteService(sshConnectionOptions(*o), req)
	if auth != nil {
		connection.Confirm, connection.Secret = auth.callbacks()
	}
	connection = withSSHDiagnostics(connection, os.Stderr, auth)
	// Cancellation covers setup. Once open, the transport lives until the client releases it.
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopSetup := context.AfterFunc(ctx, cancel)
	defer stopSetup()
	diagnostics := &shellDiagnostics{}
	pipes, err := transportio.Start(lifetime, connection, diagnostics)
	if err != nil {
		cancel()
		return nil, err
	}
	closeSSH := func() { cancel(); _ = pipes.CloseAndWait() }
	since := time.Now()
	ready, err := negotiateShellSetup(pipes, o.host, mode == "attach" && name == "")
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = remoteSetupExit(pipes)
		}
		closeSSH()
		return nil, err
	}
	// SSH setup stderr shares the screen with the terminal and later approval prompts.
	diagnostics.mu.Lock()
	diagnostics.finished = true
	diagnostics.mu.Unlock()
	record, err := remoteTaskRecord(ready)
	if err != nil {
		closeSSH()
		return nil, err
	}
	info := ready.advertisedHost()
	destination := pipes.Destination()
	var serverIP string
	if ready.Host != nil {
		serverIP = ready.Host.SSHServer
	}
	info.User, info.Address, info.Port = destination.User, destination.ObservedAddress(serverIP), destination.Port
	o.hostInfo = info
	host := o.host
	go func() {
		var shells []shellView
		if err := json.UnmarshalRead(io.LimitReader(pipes, daemonBodyLimit), &shells); err == nil && shells != nil {
			_ = reconcileParticipatingShells(host, info, shells, since)
		}
	}()
	open := func(ctx context.Context) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return openRemoteTaskEndpoint(ctx, record, pipes.DialTCP)
	}
	closeTask := closeSSH
	if mode == "new" {
		stop, done, err := claimShellOwner(lifetime, func() (net.Conn, error) { return open(ctx) }, record.UID, true)
		if err != nil {
			closeSSH()
			return nil, err
		}
		go func() { <-done; cancel() }()
		closeTask = func() { stop(); closeSSH() }
	}
	return transports.openTaskStream(ctx, o, open, closeTask)
}

type shellDiagnostics struct {
	mu       sync.Mutex
	finished bool
}

func (d *shellDiagnostics) Write(data []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.finished {
		termui.Log(os.Stderr, "", termui.Info, "%s", cleanText(strings.TrimSpace(string(data))))
	}
	return len(data), nil
}

func claimShellOwner(ctx context.Context, open func() (net.Conn, error), instance string, hosted bool) (func(), <-chan struct{}, error) {
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
	return acceptActivityLease(ctx, c, hosted)
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

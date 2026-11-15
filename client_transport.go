package main

import (
	"fmt"
	"io"
	"os"
	"time"
)

func openClientTransport(o *options) (io.ReadWriteCloser, func(), error) {
	if o.local {
		var r localSessionRecord
		var err error
		if o.command == "shell" {
			r, err = startLocalSession(o.session)
		} else {
			r, err = readLocalSession(o.session)
		}
		if err != nil {
			return nil, nil, err
		}
		if o.taskIdentity != "" && o.taskIdentity != r.identity() {
			return nil, nil, fmt.Errorf("local task changed; refresh with sshore list")
		}
		o.session = r.Name
		c, err := dialLocalSession(r)
		if err != nil {
			return nil, nil, err
		}
		_ = c.SetDeadline(time.Time{})
		return c, func() { c.Close() }, nil
	}
	cmd := sshCommand(*o)
	cmd.Stderr = os.Stderr
	reader, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	writer, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start system OpenSSH: %w", err)
	}
	sshClosed := false
	closeSSH := func() {
		if sshClosed {
			return
		}
		sshClosed = true
		_ = writer.Close()
		_ = reader.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	// The mux handshake happens after OpenSSH authentication; no local keyboard
	// reader or raw mode is active while OpenSSH prompts for passwords/host keys.
	transport := &pipeTransport{Reader: reader, Writer: writer, Closers: []io.Closer{reader, writer}}
	if err = negotiateSetup(reader, writer, o); err != nil {
		closeSSH()
		return nil, nil, err
	}
	return transport, closeSSH, nil
}

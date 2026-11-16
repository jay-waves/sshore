package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jay-waves/sshore/internal/termui"
	transportio "github.com/jay-waves/sshore/internal/transport"
)

func openClientTransport(o *options) (io.ReadWriteCloser, func(), error) {
	if o.local {
		var r localShellRecord
		var err error
		if o.command == "launch" || o.command == "spawn" {
			r, err = startLocalSession(o.session, o.owner)
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
		o.temporary = r.Owner.PID != 0
		c, err := dialLocalSession(r)
		if err != nil {
			return nil, nil, err
		}
		_ = c.SetDeadline(time.Time{})
		return c, func() { c.Close() }, nil
	}
	if o.command == "spawn" {
		return openSpawnTransport(o)
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
	transport := &transportio.Pipe{Reader: reader, Writer: writer, Closers: []io.Closer{reader, writer}}
	if err = negotiateSetup(reader, writer, o); err != nil {
		closeSSH()
		return nil, nil, err
	}
	return transport, closeSSH, nil
}

type sessionSwitch struct{ from, to options }

func (s *sessionSwitch) Error() string { return "switch session" }
func runClient(o options) (int, error) {
	previous := ""
	var fallback *sessionSwitch
	fallbackPrevious := ""
	for {
		ready := false
		code, err := runClientOnce(o, previous, func() { ready = true })
		var next *sessionSwitch
		if errors.As(err, &next) {
			fallback = next
			fallbackPrevious = previous
			previous = next.from.session
			o = next.to
			continue
		}
		if !ready && fallback != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Target unavailable; restoring %s.\n", cleanText(fallback.from.session))
			o = fallback.from
			o.command = "attach"
			o.resumePID = 0
			o.approved = nil
			o.approvalID = ""
			o.stay = false
			previous = fallbackPrevious
			fallback = nil
			continue
		}
		return code, err
	}
}

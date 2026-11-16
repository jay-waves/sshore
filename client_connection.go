package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

const recentConnectionLimit = 32

type recentConnection struct {
	Host, Session, Server, Directory string
	SSH                              []string
}

func readRecentConnections() ([]recentConnection, error) {
	dir, err := stateDir("connections")
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "recent.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var records []recentConnection
	if err = json.NewDecoder(io.LimitReader(f, 128*1024)).Decode(&records); err != nil {
		return nil, fmt.Errorf("cannot read recent connections: %w", err)
	}
	if len(records) > recentConnectionLimit {
		return nil, fmt.Errorf("too many recent connections")
	}
	for _, r := range records {
		if err := validateRecentConnection(r); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func validateRecentConnection(r recentConnection) error {
	if validSSHDestination(r.Host) != nil || validSession(r.Session) != nil || r.Server == "" || strings.ContainsAny(r.Server, "\x00\r\n") {
		return fmt.Errorf("invalid recent connection")
	}
	if r.Directory != "" && !filepath.IsAbs(r.Directory) {
		return fmt.Errorf("invalid recent connection directory")
	}
	if len(r.SSH) > 0 {
		if _, _, err := splitSSHOptions(append([]string{"--"}, r.SSH...), false); err != nil {
			return err
		}
	}
	return nil
}

func resolveRecentAttach(o *options) error {
	if o.command != "attach" {
		return nil
	}
	return applyRecentConnection(o)
}

func applyRecentConnection(o *options) error {
	records, err := readRecentConnections()
	if err != nil {
		return err
	}
	for _, r := range records {
		if o.host != "" && o.host != r.Host {
			continue
		}
		o.host = r.Host
		if o.session == "" {
			o.session = r.Session
		}
		applyConnectionOptions(o, r)
		return nil
	}
	if o.host == "" {
		return usage("no recent connection; use sshore attach HOST [NAME] or sshore launch HOST")
	}
	return nil
}

func connectionFromOptions(o options) (recentConnection, error) {
	// Forwarding flags belong to the original connection and must never replay
	// merely because the user selected a recent shell.
	ssh := transportio.WithoutForwarding(o.ssh)
	directory := o.sshDirectory
	if len(ssh) == 0 {
		directory = ""
	} else if directory == "" {
		var err error
		directory, err = os.Getwd()
		if err != nil {
			return recentConnection{}, err
		}
	}
	server := o.server
	if server == "" {
		server = "sshore"
	}
	return recentConnection{Directory: directory, Host: o.host, Session: o.session, Server: server, SSH: ssh}, nil
}

func applyConnectionOptions(o *options, r recentConnection) {
	if !o.serverSet {
		o.server = r.Server
	}
	if len(o.ssh) == 0 {
		o.ssh = append([]string(nil), r.SSH...)
		o.sshDirectory = r.Directory
	}
}

func rememberConnection(o options) error {
	if o.plain || o.host == "" || o.session == "" {
		return nil
	}
	records, err := readRecentConnections()
	if err != nil {
		return err
	}
	record, err := connectionFromOptions(o)
	if err != nil {
		return err
	}
	next := []recentConnection{record}
	for _, old := range records {
		if old.Host != o.host && len(next) < recentConnectionLimit {
			next = append(next, old)
		}
	}
	dir, err := stateDir("connections")
	if err != nil {
		return err
	}
	return writeStateJSON(filepath.Join(dir, "recent.json"), next)
}

// rememberShellRename changes cached labels only after a successful remote rename.
// It does not reorder recent hosts or treat rename as an attachment confirmation.
func rememberShellRename(host, old, name string) error {
	if validSSHDestination(host) != nil || validSession(old) != nil || validSession(name) != nil {
		return fmt.Errorf("invalid shell rename identity")
	}
	if old == name {
		return nil
	}
	snapshotErr := renameShellSnapshot(host, old, name)
	records, err := readRecentConnections()
	if err != nil {
		return errors.Join(snapshotErr, err)
	}
	changed := false
	for i := range records {
		if records[i].Host == host && records[i].Session == old {
			records[i].Session = name
			changed = true
		}
	}
	if changed {
		dir, e := stateDir("connections")
		if e != nil {
			return errors.Join(snapshotErr, e)
		}
		err = writeStateJSON(filepath.Join(dir, "recent.json"), records)
	}
	return errors.Join(snapshotErr, err)
}

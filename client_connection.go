package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

func openClientTransport(o *options) (io.ReadWriteCloser, func(), error) {
	return openClientTransportContext(context.Background(), o)
}

func openClientTransportContext(ctx context.Context, o *options) (wire io.ReadWriteCloser, closeTransport func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	if o.routeParent != nil {
		parent := routeParentOptions(*o.routeParent)
		c, closeParent, err := openClientTransportContext(ctx, &parent)
		if err != nil {
			return nil, nil, err
		}
		if err = writeMessage(c, message{Type: "spawn_route_connect", Token: o.routeToken, Instance: parent.instance, SoftwareVersion: version}); err == nil {
			var reply message
			reply, err = readMessage(c)
			if err == nil && reply.Type != "route_connected" {
				err = fmt.Errorf("spawn route: %s", reply.Error)
			}
		}
		if err != nil {
			closeParent()
			return nil, nil, err
		}
		if o.routeTarget {
			if err = writeMessage(c, message{Type: "shell_connect", Action: o.command, Name: o.session, Instance: o.instance, SoftwareVersion: version}); err == nil {
				var reply message
				reply, err = readMessage(c)
				if err == nil && reply.Type != "setup_ready" {
					err = fmt.Errorf("shell connection: %s", reply.Error)
				}
			}
			if err != nil {
				closeParent()
				return nil, nil, err
			}
		}
		return c, closeParent, nil
	}
	if o.local {
		var r localShellRecord
		var err error
		if o.command == "launch" || o.command == "spawn" {
			r, err = startLocalSessionContext(ctx, *o)
		} else {
			name := o.session
			if strings.HasPrefix(o.taskIdentity, "task:") {
				name = strings.TrimPrefix(o.taskIdentity, "task:")
			}
			r, err = readLocalSession(name)
		}
		if err != nil {
			return nil, nil, err
		}
		if o.taskIdentity != "" && o.taskIdentity != r.identity() {
			return nil, nil, fmt.Errorf("local task changed; refresh with sshore list")
		}
		o.taskIdentity = r.identity()
		o.session = r.Name
		o.temporary = r.Owner.PID != 0
		c, err := dialLocalSessionContext(ctx, r, time.Now().Add(5*time.Second))
		if err != nil {
			return nil, nil, err
		}
		_ = c.SetDeadline(time.Time{})
		stop := context.AfterFunc(ctx, func() { c.Close() })
		return c, func() { stop(); c.Close() }, nil
	}
	if o.command == "spawn" {
		return openSpawnTransport(ctx, o)
	}
	process, err := transportio.Start(ctx, sshConnectionOptions(*o), os.Stderr)
	if err != nil {
		return nil, nil, err
	}
	reader, writer := process.Output, process.Input
	closeSSH := func() { process.Close(); process.Wait() }
	// The mux handshake follows SSH authentication; terminal raw mode and its
	// keyboard reader start only after authentication and setup finish.
	transport := process
	if err = negotiateSetupUsing(ctx, reader, writer, o, setupClientUI{OpenInput: func() (setupInput, error) { return newApprovalInput() }}); err != nil {
		closeSSH()
		return nil, nil, err
	}
	if o.daemonSocket != "" && !o.plain {
		channel, err := process.DialSocket(ctx, o.daemonSocket)
		if err != nil {
			closeSSH()
			return nil, nil, fmt.Errorf("connect remote daemon: %w", err)
		}
		return channel, func() { channel.Close(); closeSSH() }, nil
	}
	return transport, closeSSH, nil
}

type sessionSwitch struct {
	from, to    options
	returnLocal bool
}

func (s *sessionSwitch) Error() string { return "switch session" }
func runClient(o options) (int, error) {
	previous := ""
	type returnTarget struct {
		options  options
		previous string
	}
	var returns []returnTarget
	var fallback *sessionSwitch
	fallbackPrevious := ""
	for {
		ready := false
		code, err := runClientOnce(o, previous, func() { ready = true; o.handoffDone = nil })
		if !ready && o.handoffDone != nil {
			_ = o.handoffDone(err)
			o.handoffDone = nil
		}
		var next *sessionSwitch
		if errors.As(err, &next) {
			if next.returnLocal {
				returns = append(returns, returnTarget{next.from, previous})
			}
			fallback = next
			fallbackPrevious = previous
			previous = next.from.session
			if next.returnLocal {
				previous = ""
			}
			o = next.to
			continue
		}
		if !ready && fallback != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Target unavailable; restoring %s.\n", cleanText(fallback.from.session))
			if fallback.returnLocal && len(returns) > 0 {
				returns = returns[:len(returns)-1]
			}
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
		if len(returns) > 0 {
			if err != nil {
				termui.Log(os.Stderr, termui.Warning, "[sshore] %s\n", cleanText(err.Error()))
			}
			target := returns[len(returns)-1]
			returns = returns[:len(returns)-1]
			o = target.options
			o.command, o.resumePID = "attach", 0
			fallback = nil
			previous = target.previous
			continue
		}
		return code, err
	}
}

const recentConnectionLimit = 32

type recentConnection struct {
	RouteTarget                      bool              `json:",omitempty"`
	Parent                           *recentConnection `json:",omitempty"`
	RouteToken                       string            `json:",omitempty"`
	Instance                         string            `json:",omitempty"`
	Host, Session, Server, Directory string
	SSH                              []string
}

func readRecentConnections() ([]recentConnection, error) {
	dir, err := stateDir("connections")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "recent.json")
	if err := privateStatePath(path, false); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
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
	return validateConnectionDepth(r, 0)
}

func validateConnectionDepth(r recentConnection, depth int) error {
	if depth >= 64 {
		return fmt.Errorf("too many connection hops")
	}
	if r.Parent != nil {
		token, err := hex.DecodeString(r.RouteToken)
		if err != nil || len(token) != 32 || r.RouteToken != strings.ToLower(r.RouteToken) || r.Instance == "" || r.Parent.Instance == "" {
			return fmt.Errorf("invalid saved spawn route")
		}
		if err := validateConnectionDepth(*r.Parent, depth+1); err != nil {
			return err
		}
	} else if r.RouteToken != "" || r.RouteTarget {
		return fmt.Errorf("spawn route parent missing")
	}
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
	if o.host != "" && o.session != "" {
		rows, err := readShellSnapshots()
		if err != nil {
			return err
		}
		var saved *recentConnection
		for _, row := range rows {
			if row.Host != o.host || row.Name != o.session || row.State == "renamed" || (o.instance != "" && row.Instance != o.instance) {
				continue
			}
			if row.Connection == nil {
				continue
			}
			if saved != nil && saved.Instance != row.Instance && (saved.Parent != nil || row.Connection.Parent != nil) {
				return fmt.Errorf("ambiguous shell route; select the shell by task ID")
			}
			saved = row.Connection
		}
		if saved != nil && saved.Parent != nil {
			applyConnectionOptions(o, *saved)
			return nil
		}
	}

	records, err := readRecentConnections()
	if err != nil {
		return err
	}
	for _, r := range records {
		if o.host != "" && o.host != r.Host {
			continue
		}
		if r.Parent != nil && o.session != "" && o.session != r.Session {
			return fmt.Errorf("no saved route to %s/%s; attach from its parent shell", cleanText(o.host), cleanText(o.session))
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
	return connectionFromOptionsDepth(o, 0)
}

func connectionFromOptionsDepth(o options, depth int) (recentConnection, error) {
	if depth >= 64 {
		return recentConnection{}, fmt.Errorf("too many connection hops")
	}
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
	record := recentConnection{Directory: directory, Host: o.host, Session: o.session, Server: server, SSH: ssh, Instance: o.instance}
	if o.routeParent != nil {
		parent, err := connectionFromOptionsDepth(*o.routeParent, depth+1)
		if err != nil {
			return recentConnection{}, err
		}
		record.Parent, record.RouteToken, record.RouteTarget = &parent, o.routeToken, o.routeTarget
		if err := validateConnectionDepth(record, depth); err != nil {
			return recentConnection{}, err
		}
	}
	return record, nil
}

func applyConnectionOptions(o *options, r recentConnection) {
	if r.Parent != nil {
		parent := options{command: "attach", host: r.Parent.Host, session: r.Parent.Session}
		applyConnectionOptions(&parent, *r.Parent)
		o.routeParent, o.routeToken, o.routeTarget = &parent, r.RouteToken, r.RouteTarget
	}
	if o.instance == "" && (o.session == "" || o.session == r.Session) {
		o.instance = r.Instance
	}

	if !o.serverSet {
		o.server = r.Server
	}
	if o.portOverrideOnly {
		flags := sshOptionKinds(true)
		for i := 0; i < len(r.SSH); {
			end := i + 1
			if flags[r.SSH[i]] != cliBool {
				end++
			}
			if end > len(r.SSH) {
				break
			}
			if end == i+1 || !sshPortSetting(r.SSH[i], r.SSH[i+1]) {
				o.ssh = append(o.ssh, r.SSH[i:end]...)
			}
			i = end
		}
		o.sshDirectory = r.Directory
		o.portOverrideOnly = false
	} else if len(o.ssh) == 0 {
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

func routeParentOptions(parent options) options {
	parent.command = "attach"
	// Bootstrap accepts immutable task UIDs, so an ancestor rename cannot sever
	// an otherwise live child route. This transport never attaches a terminal.
	if validTaskUID(parent.instance) {
		parent.session = parent.instance
	}
	return parent
}

func confirmSpawnRoute(parent options, token string, result error) error {
	parent = routeParentOptions(parent)
	c, closeTransport, err := openClientTransport(&parent)
	if err != nil {
		return err
	}
	defer closeTransport()
	request := message{Type: "spawn_route_confirm", Token: token, Instance: parent.instance, SoftwareVersion: version}
	if result != nil {
		request.Error = cleanText(result.Error())
	}
	if err := writeMessage(c, request); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return err
	}
	if reply.Type != "route_confirmed" {
		return fmt.Errorf("spawn confirmation: %s", reply.Error)
	}
	return nil
}

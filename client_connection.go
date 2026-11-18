package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/wire"
	"golang.org/x/term"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The frontend retains local daemon leases across attachment changes.
// Saved routes get new leases and authenticate normally.
type clientTransports map[*clientTransport]bool

type clientTransport struct {
	open              func(context.Context) (io.ReadWriteCloser, error)
	close             func()
	session, instance string
}

func (ts clientTransports) releaseExcept(keep clientTransports) {
	for t := range ts {
		if !keep[t] {
			delete(ts, t)
			t.close()
		}
	}
}

// A routed target may still need its ancestor's authenticated transport.
func keepRoute(keep clientTransports, o *options) {
	for ; o != nil; o = o.routeParent {
		if o.reusable != nil {
			keep[o.reusable] = true
		}
	}
}
func openTaskStream(ctx context.Context, o *options, open func(context.Context) (io.ReadWriteCloser, error), closeTask func()) (io.ReadWriteCloser, error) {
	stream, err := open(ctx)
	if err != nil {
		closeTask()
		return nil, err
	}
	if o.transports == nil {
		return wire.OwnTransport(ctx, stream, closeTask), nil
	}
	t := &clientTransport{open: open, close: closeTask}
	o.reusable = t
	o.transports[t] = true
	return wire.OwnTransport(ctx, stream, nil), nil
}

func openReusableTransport(ctx context.Context, connection *clientTransport) (io.ReadWriteCloser, error) {
	c, err := connection.open(ctx)
	if err != nil {
		return nil, err
	}
	return wire.OwnTransport(ctx, c, nil), nil
}

// The returned stream owns its cleanup. Reused attachments release only their
// own stream. A retained daemon lease can open another stream without reauthenticating.
func openClientTransportContext(ctx context.Context, o *options) (stream io.ReadWriteCloser, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	if o.reusable != nil && o.command == "attach" &&
		(o.instance != "" && o.instance == o.reusable.instance || o.instance == "" && o.session == o.reusable.session) {
		return openReusableTransport(ctx, o.reusable)
	}
	o.reusable = nil
	if o.routeParent != nil {
		parent := routeParentOptions(*o.routeParent)
		c, err := openClientTransportContext(ctx, &parent)
		if err != nil {
			return nil, err
		}
		if err = writeMessage(c, message{Type: "spawn_route_connect", Token: o.routeToken, Instance: parent.instance, SoftwareVersion: version}); err == nil {
			var reply message
			reply, err = readMessage(c)
			if err == nil && reply.Type != "route_connected" {
				err = fmt.Errorf("spawn route: %s", reply.Error)
			}
		}
		if err != nil {
			c.Close()
			return nil, err
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
				c.Close()
				return nil, err
			}
		}
		return c, nil
	}
	if o.local {
		if o.hosted {
			c, err := openHostedTransport(ctx, *o)
			if err != nil {
				return nil, err
			}
			return wire.OwnTransport(ctx, c, nil), nil
		}
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
			return nil, err
		}
		if o.taskIdentity != "" && o.taskIdentity != r.identity() {
			return nil, fmt.Errorf("local task changed; refresh with sshore list")
		}
		o.taskIdentity = r.identity()
		o.session = r.Name
		o.temporary = r.Owner.PID != 0
		c, err := dialLocalSessionContext(ctx, r, time.Now().Add(5*time.Second))
		if err != nil {
			return nil, err
		}
		_ = c.SetDeadline(time.Time{})
		return wire.OwnTransport(ctx, c, nil), nil
	}
	return openRemoteShellTransport(ctx, o)
}

// SSH carries TCP channels to the same loopback endpoint used by native clients.
func openRemoteDaemonTask(ctx context.Context, dial func(context.Context, string) (net.Conn, error), record localShellRecord) (net.Conn, error) {
	host, _, err := net.SplitHostPort(record.Address)
	if err != nil || host != "127.0.0.1" || !validTaskUID(record.UID) || len(record.Token) != 64 {
		return nil, fmt.Errorf("invalid daemon endpoint")
	}
	authCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := dial(authCtx, record.Address)
	if err != nil {
		return nil, err
	}
	return upgradeDaemonConn(authCtx,c,record,"/tasks/"+record.UID+"/open",nil)
}

type sessionSwitch struct {
	from, to    options
	returnLocal bool
}

func (s *sessionSwitch) Error() string { return "switch session" }
func runClient(o options) (int, error) {
	transports := make(clientTransports)
	defer transports.releaseExcept(nil)
	// Keep the frontend alive while an attempt unwinds and restores its parent.
	// Each attempt routes interrupts according to its connection phase.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	previous := ""
	type returnTarget struct {
		options  options
		previous string
	}
	var returns []returnTarget
	var fallback *sessionSwitch
	fallbackPrevious := ""
	prune := func(current *options) {
		keep := make(clientTransports)
		keepRoute(keep, current)
		if fallback != nil {
			keepRoute(keep, &fallback.from)
		}
		for i := range returns {
			keepRoute(keep, &returns[i].options)
		}
		transports.releaseExcept(keep)
	}
	for {
		prune(&o)
		o.transports = transports
		ready := false
		code, err := runClientOnce(o, previous, func(current options) {
			ready = true
			o.handoffDone = nil
			fallback = nil
			prune(&current)
		})
		if !ready && o.handoffDone != nil {
			_ = o.handoffDone(context.Background(), err)
			o.handoffDone = nil
		}
		if next, ok := errors.AsType[*sessionSwitch](err); ok {
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
			o.recovering = true
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
			o.command = "attach"
			o.recovering = true
			fallback = nil
			previous = target.previous
			continue
		}
		return code, err
	}
}

const recentConnectionLimit = 32

type recentConnection struct {
	Hosted                           bool              `json:",omitempty"`
	RouteTarget                      bool              `json:",omitempty"`
	Parent                           *recentConnection `json:",omitempty"`
	RouteToken                       string            `json:",omitempty"`
	Instance                         string            `json:",omitempty"`
	Host, Session, Server, Directory string
	SSH                              []string
}

func (r recentConnection) viaShell() bool { return r.Hosted || r.Parent != nil }

func (c recentConnection) clone() recentConnection {
	c.SSH = append([]string(nil), c.SSH...)
	if c.Parent != nil {
		parent := c.Parent.clone()
		c.Parent = &parent
	}
	return c
}

func readRecentConnections() ([]recentConnection, error) {
	r, err := metadataRPC(metadataRequest{Action: "connection_get"})
	return r.Connections, err
}
func readRecentConnectionsLocal() (result []recentConnection, resultErr error) {
	hub := &accountActivity.Load().metadata
	root, err := statePath("connections")
	if err != nil {
		return nil, err
	}
	if hub.connectionRoot == root {
		records := make([]recentConnection, len(hub.connections))
		for i, record := range hub.connections {
			records[i] = record.clone()
		}
		return records, nil
	}
	defer func() {
		if resultErr == nil {
			hub.connectionRoot = root
			hub.connections = append([]recentConnection(nil), result...)
		}
	}()
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
	if r.Hosted && (r.Host != "local" || !validTaskUID(r.Instance) || len(r.SSH) != 0 || r.Parent != nil) {
		return fmt.Errorf("invalid hosted shell connection")
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
	record := recentConnection{Hosted: o.hosted, Directory: directory, Host: o.host, Session: o.session, Server: server, SSH: ssh, Instance: o.instance}
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
	if r.Hosted {
		o.local, o.hosted = true, true
		o.taskIdentity = "task:" + r.Instance
	}
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
	if o.host == "" || o.session == "" {
		return nil
	}
	record, err := connectionFromOptions(o)
	if err != nil {
		return err
	}
	_, err = metadataRPC(metadataRequest{Action: "connection_save", Connection: &record})
	return err
}
func rememberConnectionLocal(record recentConnection) error {
	if err := validateRecentConnection(record); err != nil {
		return err
	}
	record = record.clone()
	records, err := readRecentConnectionsLocal()
	if err != nil {
		return err
	}
	next := []recentConnection{record}
	for _, old := range records {
		if old.Host != record.Host && len(next) < recentConnectionLimit {
			next = append(next, old)
		}
	}
	return writeRecentConnectionsLocal(next)
}
func writeRecentConnectionsLocal(records []recentConnection) error {
	dir, err := stateDir("connections")
	if err != nil {
		return err
	}
	accountActivity.Load().metadata.connections = records
	return writeStateJSON(filepath.Join(dir, "recent.json"), records)
}

// rememberShellRename changes cached labels only after a successful remote rename.
// It does not reorder recent hosts or treat rename as an attachment confirmation.
func rememberShellRename(host, old, name string) error {
	_, err := metadataRPC(metadataRequest{Action: "connection_rename", Host: host, Key: old, Name: name})
	return err
}
func rememberShellRenameLocal(host, old, name string) error {
	if validSSHDestination(host) != nil || validSession(old) != nil || validSession(name) != nil {
		return fmt.Errorf("invalid shell rename identity")
	}
	if old == name {
		return nil
	}
	snapshotErr := renameShellSnapshotLocked(host, old, name)
	records, err := readRecentConnectionsLocal()
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
		err = writeRecentConnectionsLocal(records)
	}
	return errors.Join(snapshotErr, err)
}

func routeParentOptions(parent options) options {
	parent.command = "attach"
	// A route borrows an existing connection. A fresh dial belongs to this stream.
	parent.transports = nil
	// Bootstrap accepts immutable task UIDs, so an ancestor rename cannot sever
	// an otherwise live child route. This transport never attaches a terminal.
	if validTaskUID(parent.instance) {
		parent.session = parent.instance
	}
	return parent
}

func confirmSpawnRoute(ctx context.Context, parent options, token string, result error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	parent = routeParentOptions(parent)
	c, err := openClientTransportContext(ctx, &parent)
	if err != nil {
		return err
	}
	defer c.Close()
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

// Only local authenticated control carries this request; terminal output never does.
type clientHandoff struct {
	LocalAttach            bool
	Hosted                 bool
	Args                   []string
	Owner                  processIdentity
	Directory              string
	Environment            []string
	Tree                   []shellTreeNode
	TaskIdentity, Instance string
}

func (h *clientHandoff) options() (options, error) {
	if h == nil {
		return options{}, fmt.Errorf("missing handoff request")
	}
	var o options
	var err error
	if h.LocalAttach {
		if len(h.Args) == 0 || h.Args[0] != "attach" {
			return o, fmt.Errorf("invalid local attachment handoff")
		}
		parsed, parseErr := parseCLI(h.Args[1:], "attach")
		if parseErr != nil {
			return o, parseErr
		}
		if len(parsed.operands) != 1 || len(parsed.ssh) != 0 || parsed.has("--server") || parsed.has("--download-dir") {
			return o, fmt.Errorf("invalid local attachment handoff")
		}
		// Scope is authenticated handoff metadata, not a public CLI selector.
		parsed.values["--local"] = ""
		o, err = shellOptions("attach", parsed)
	} else {
		o, err = parseOptions(h.Args)
	}
	if err != nil {
		return o, err
	}
	if o.detached || (o.command != "spawn" && o.command != "launch" && o.command != "attach") || !filepath.IsAbs(h.Directory) {
		return o, fmt.Errorf("invalid remote handoff")
	}
	if h.Hosted && (!o.local || o.command != "attach" || !validTaskUID(h.Instance) || h.TaskIdentity != "task:"+h.Instance) {
		return o, fmt.Errorf("invalid hosted shell handoff")
	}
	if o.command == "spawn" {
		identity, err := spawnProcessIdentity(h.Owner.PID)
		if h.Owner.PID <= 1 || err != nil || identity != h.Owner.Start {
			return o, fmt.Errorf("spawn owner exited")
		}
		o.temporary, o.owner = true, h.Owner
	}
	o.sshDirectory, o.environment, o.tree = h.Directory, h.Environment, h.Tree
	o.taskIdentity, o.instance = h.TaskIdentity, h.Instance
	o.hosted = h.Hosted
	return o, nil
}

func handoffLocalClient(o options) (bool, error) {
	identity := os.Getenv("SSHORE_LOCAL_SESSION")
	if identity == "" || o.detached || (o.command != "spawn" && o.command != "launch" && o.command != "attach") || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return false, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return true, err
	}
	if o.sshDirectory != "" {
		cwd = o.sshDirectory
	}
	args := []string{o.command}
	if o.local {
		if o.command != "attach" {
			args = append(args, "--local")
		}
	} else {
		args = append(args, o.host)
	}
	if o.session != "" {
		args = append(args, o.session)
	}
	if !o.local {
		args = append(args, "--server", o.server)
	}
	if o.downloadDir != "" {
		args = append(args, "--download-dir", o.downloadDir)
	}
	for _, flag := range []struct {
		name    string
		enabled bool
	}{{"--quiet", o.quiet}, {"--no-escape", o.noEscape}, {"--no-login", o.noLogin}, {"--no-profile", o.noProfile}} {
		if flag.enabled {
			args = append(args, flag.name)
		}
	}
	if len(o.ssh) > 0 {
		args = append(append(args, "--"), o.ssh...)
	}
	environment := os.Environ()
	if !o.local {
		// Resolve the initiating shell's executable before handing off.
		ssh, err := exec.LookPath(sshExecutable())
		if err != nil {
			return true, fmt.Errorf("system OpenSSH: %w", err)
		}
		ssh, err = filepath.Abs(ssh)
		if err != nil {
			return true, err
		}
		environment = append(environment, "SSHORE_SSH="+ssh)
	}
	h := &clientHandoff{LocalAttach: o.local && o.command == "attach", Hosted: o.hosted, Args: args, Owner: o.owner, Directory: cwd, Environment: environment, Tree: currentShellTree(), TaskIdentity: o.taskIdentity, Instance: o.instance}
	rows, err := localShells()
	if err != nil {
		return true, err
	}
	for _, row := range rows {
		if row.identity != identity {
			continue
		}
		m, err := localRequest(row.Name, message{Type: "handoff", Handoff: h}, identity)
		if err != nil {
			return true, err
		}
		if m.Type != "handoff_accepted" {
			return true, fmt.Errorf("remote handoff: %s", m.Error)
		}
		return true, nil
	}
	return true, fmt.Errorf("local shell is unavailable for remote handoff")
}

type localHandoff struct {
	token      string
	attachment *attachment
	result     chan message
}

func (s *localShell) requestHandoff(c net.Conn, h *clientHandoff) error {
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	if _, err := h.options(); err != nil {
		return writeMessage(c, message{Type: "error", Error: err.Error()})
	}
	s.mu.Lock()
	p := s.lifecycle.Active()
	if p == nil || s.handoff != nil {
		s.mu.Unlock()
		return writeMessage(c, message{Type: "error", Error: "local frontend unavailable or busy"})
	}
	pending := &localHandoff{token: resourceToken(), attachment: p, result: make(chan message, 1)}
	s.handoff = pending
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.handoff == pending {
			s.handoff = nil
		}
		s.mu.Unlock()
	}()
	if err := p.controlMessage(message{Type: "handoff", Handoff: h, Token: pending.token}); err != nil {
		return err
	}
	select {
	case result := <-pending.result:
		if result.Error != "" {
			return writeMessage(c, message{Type: "error", Error: result.Error})
		}
		if err := p.controlMessage(message{Type: "handoff_commit", Token: pending.token}); err != nil {
			return err
		}
		select {
		case result := <-pending.result:
			if result.Error != "" {
				return writeMessage(c, message{Type: "error", Error: result.Error})
			}
			return writeMessage(c, message{Type: "handoff_accepted"})
		case <-time.After(2 * time.Minute):
			return writeMessage(c, message{Type: "error", Error: "attachment startup timed out"})
		}
	case <-p.Done():
		return writeMessage(c, message{Type: "error", Error: "local frontend disconnected"})
	case <-time.After(4 * time.Second):
		return writeMessage(c, message{Type: "error", Error: "local frontend handoff timed out"})
	}
}

func (s *localShell) finishHandoff(p *attachment, m message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.handoff; h != nil && h.attachment == p && h.token == m.Token {
		select {
		case h.result <- m:
		default:
		}
	}
}

func (s *localShell) confirmHandoff(c net.Conn, m message) error {
	s.mu.Lock()
	pending := s.handoff
	if pending == nil || pending.token != m.Token {
		s.mu.Unlock()
		return writeMessage(c, message{Type: "error", Error: "handoff expired"})
	}
	select {
	case pending.result <- m:
	default:
	}
	s.mu.Unlock()
	return writeMessage(c, message{Type: "handoff_confirmed"})
}

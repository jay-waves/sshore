package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

// The reader remains with the lease stream when a handshake switches to raw I/O.
type clientStream struct {
	net.Conn
	reader      *bufio.Reader
	closeStream func()
	stop        func() bool
}

func ownClientTransport(ctx context.Context, stream net.Conn, cleanup func()) *clientStream {
	closeStream := sync.OnceFunc(func() {
		_ = stream.Close()
		if cleanup != nil {
			cleanup()
		}
	})
	return &clientStream{Conn: stream, reader: bufio.NewReader(stream), closeStream: closeStream, stop: context.AfterFunc(ctx, closeStream)}
}

func (c *clientStream) Read(b []byte) (int, error)           { return c.reader.Read(b) }
func (c *clientStream) ReadSlice(delim byte) ([]byte, error) { return c.reader.ReadSlice(delim) }
func (c *clientStream) Close() error {
	c.stop()
	c.closeStream()
	return nil
}

// The frontend owns these daemon leases. A true entry marks a lease referenced
// by the current, pending or parent route during a sweep.
type clientTransports map[*clientTransport]bool

type clientTransport struct {
	open              func(context.Context) (net.Conn, error)
	close             func()
	session, instance string
}

func (ts clientTransports) releaseUnreferenced() {
	for t, keep := range ts {
		if keep {
			ts[t] = false
			continue
		}
		delete(ts, t)
		t.close()
	}
}

// A routed target may still need its ancestor's authenticated transport.
func (ts clientTransports) retainRoute(o *options) {
	for ; o != nil; o = o.routeParent {
		if _, owned := ts[o.reusable]; owned {
			ts[o.reusable] = true
		}
	}
}
func (ts clientTransports) openTaskStream(ctx context.Context, o *options, open func(context.Context) (net.Conn, error), closeTask func()) (net.Conn, error) {
	stream, err := open(ctx)
	if err != nil {
		closeTask()
		return nil, err
	}
	if ts == nil {
		return ownClientTransport(ctx, stream, closeTask), nil
	}
	t := &clientTransport{open: open, close: closeTask, session: o.session, instance: o.instance}
	o.reusable = t
	ts[t] = false
	return ownClientTransport(ctx, stream, nil), nil
}

func openReusableTransport(ctx context.Context, connection *clientTransport) (net.Conn, error) {
	c, err := connection.open(ctx)
	if err != nil {
		return nil, err
	}
	return ownClientTransport(ctx, c, nil), nil
}

// The returned stream owns its cleanup. Reused attachments release only their
// own stream. A retained daemon lease can open another stream without reauthenticating.
func openClientTransportContext(ctx context.Context, o *options, transports clientTransports) (stream net.Conn, err error) {
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
		return openSessionRoute(ctx, o, transports)
	}
	if o.local {
		if o.hosted {
			c, err := openHostedTransport(ctx, *o)
			if err != nil {
				return nil, err
			}
			_ = c.SetDeadline(time.Time{})
			return ownClientTransport(ctx, c, nil), nil
		}
		var c net.Conn
		var r localShellRecord
		var err error
		if o.command == "launch" || o.command == "spawn" {
			r, err = startLocalSessionContext(ctx, *o)
			if err == nil {
				if o.taskIdentity != "" && o.taskIdentity != r.identity() {
					return nil, fmt.Errorf("local task changed; refresh with sshore list")
				}
				c, err = dialLocalSessionContext(ctx, r, time.Now().Add(5*time.Second))
			}
		} else {
			ref := o.session
			if canonicalTaskUID(o.taskIdentity) != "" {
				ref = ""
			}
			c, r, err = openNativeShell(ctx, ref, o.taskIdentity)
		}
		if err != nil {
			return nil, err
		}
		o.taskIdentity = r.identity()
		o.session = r.Name
		o.temporary = r.Owner.PID != 0
		_ = c.SetDeadline(time.Time{})
		return ownClientTransport(ctx, c, nil), nil
	}
	return openRemoteShellTransport(ctx, o, transports)
}

type returnTarget struct {
	options  options
	previous string
}

type sessionSwitch struct {
	from           returnTarget
	to             options
	returnToParent bool
}

func (s *sessionSwitch) Error() string { return "switch session" }
func runClient(o options) (int, error) {
	transports := make(clientTransports)
	observations := make(map[*handoffObservation]struct{})
	defer func() {
		for r := range observations {
			r.conn.Close()
		}
	}()
	defer transports.releaseUnreferenced()
	// Keep the frontend alive while an attempt unwinds and restores its parent.
	// Each attempt routes interrupts according to its connection phase.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	previous := ""
	var parents []returnTarget
	var pending *sessionSwitch
	returning := false
	returnCode := 0
	prune := func(current *options) {
		transports.retainRoute(current)
		if pending != nil {
			transports.retainRoute(&pending.from.options)
		}
		for i := range parents {
			transports.retainRoute(&parents[i].options)
		}
		transports.releaseUnreferenced()
	}
	for {
		prune(&o)
		code, err := runClientOnce(o, previous, transports, func(current options) {
			o.handoffDone = nil
			returning = false
			if current.handoffHint != nil {
				current.handoffHint.conn.Close()
				delete(observations, current.handoffHint)
				o.handoffHint = nil
			}
			if pending != nil {
				if pending.returnToParent {
					parents = append(parents, pending.from)
				}
				pending = nil
			}
			prune(&current)
		})
		if o.handoffDone != nil {
			_ = o.handoffDone(context.Background(), err)
			o.handoffDone = nil
		}
		if command, ok := errors.AsType[*consoleResourceCommand](err); ok {
			signal.Stop(signals)
			return command.run()
		}
		if next, ok := errors.AsType[*sessionSwitch](err); ok {
			pending = next
			if next.from.options.handoffHint != nil {
				observations[next.from.options.handoffHint] = struct{}{}
			}
			source := next.from.options
			switch {
			case next.returnToParent:
				previous = ""
			case source.local && !source.hosted:
				previous = canonicalTaskUID(source.taskIdentity)
			default:
				previous = source.instance
			}
			o = next.to
			continue
		}
		var target returnTarget
		switch {
		case pending != nil:
			target = pending.from
			pending = nil
			termui.Log(os.Stderr, termui.Warning, "[sshore] Target unavailable; restoring %s.\n", cleanText(target.options.session))
		case returning && err != nil:
			termui.Log(os.Stderr, termui.Warning, "[sshore] Could not return to %s: %s. Leaving this frontend.\n", cleanText(o.session), cleanText(err.Error()))
			return returnCode, nil
		case len(parents) == 0:
			return code, err
		default:
			last := len(parents) - 1
			target = parents[last]
			parents = slices.Delete(parents, last, last+1)
			if err != nil {
				termui.Log(os.Stderr, termui.Warning, "[sshore] %s\n", cleanText(err.Error()))
			}
		}
		returning, returnCode = true, code
		o, previous = target.options, target.previous
		o.command, o.recovering = "attach", true
	}
}

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

const maxConnectionHops = 64

func (c *recentConnection) clone() *recentConnection {
	if c == nil {
		return nil
	}
	cloned := *c
	for hop := &cloned; hop != nil; hop = hop.Parent {
		hop.SSH = append([]string(nil), hop.SSH...)
		if hop.Parent != nil {
			parent := *hop.Parent
			hop.Parent = &parent
		}
	}
	return &cloned
}

func readRecentConnections() ([]recentConnection, error) {
	r, err := metadataRPC(metadataRequest{Action: "connection_get"})
	return r.Connections, err
}
func (m *daemonMetadata) readRecentConnectionsLocal() ([]recentConnection, error) {
	if err := m.connections.load("recent.json", 128*1024, validateRecentConnection); err != nil {
		return nil, fmt.Errorf("cannot read recent connections: %w", err)
	}
	return m.connections.rows, nil
}

func validateRecentConnection(r recentConnection) error {
	for depth, hop := 0, &r; hop != nil; depth, hop = depth+1, hop.Parent {
		if depth >= maxConnectionHops {
			return fmt.Errorf("too many connection hops")
		}
		if hop.Hosted && (hop.Host != "local" || !validTaskUID(hop.Instance) || len(hop.SSH) != 0 || hop.Parent != nil) {
			return fmt.Errorf("invalid hosted shell connection")
		}
		if hop.Parent != nil {
			token := hop.RouteToken
			if len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" || hop.Instance == "" || hop.Parent.Instance == "" {
				return fmt.Errorf("invalid saved spawn route")
			}
		} else if hop.RouteToken != "" || hop.RouteTarget {
			return fmt.Errorf("spawn route parent missing")
		}
		if validSSHDestination(hop.Host) != nil || validSession(hop.Session) != nil || hop.Server == "" || strings.ContainsAny(hop.Server, "\x00\r\n") {
			return fmt.Errorf("invalid recent connection")
		}
		if hop.Directory != "" && !filepath.IsAbs(hop.Directory) {
			return fmt.Errorf("invalid recent connection directory")
		}
		if len(hop.SSH) > 0 {
			if _, _, err := parseSSHOptions(hop.SSH, false); err != nil {
				return err
			}
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
	reply, err := metadataRPC(metadataRequest{Action: "connection_select", Host: o.host, Name: o.session, Instance: o.instance})
	if err != nil {
		return err
	}
	if r := reply.Connection; r != nil {
		o.host = r.Host
		if o.session == "" {
			o.session = r.Session
		}
		applyConnectionOptions(o, *r)
		return nil
	}
	if o.host == "" {
		return usage("no recent connection; use sshore attach HOST:[ID] or sshore launch HOST:")
	}
	return nil
}

// Resolve shell history and the recent host fallback under the same metadata lock.
func (a *daemonActivity) selectRecentConnection(host, name, instance string) (*recentConnection, error) {
	if host != "" && name != "" {
		var routed *recentConnection
		var firstInstance string
		found, ambiguous := false, false
		err := a.walkShellSnapshotsLocked(func(s shellSnapshot) {
			if s.Host != host || s.Name != name || s.State == "renamed" || s.Connection == nil ||
				(instance != "" && s.Instance != instance) {
				return
			}
			if !found {
				firstInstance, found = s.Instance, true
			} else if s.Instance != firstInstance {
				ambiguous = true
			}
			if s.Connection.Parent != nil {
				routed = s.Connection
			}
		})
		if err != nil {
			return nil, err
		}
		if routed != nil {
			if ambiguous {
				return nil, fmt.Errorf("ambiguous shell route; select the shell by task ID")
			}
			return routed.clone(), nil
		}
	}

	records, err := a.metadata.readRecentConnectionsLocal()
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		if host != "" && host != r.Host {
			continue
		}
		if r.Parent != nil && name != "" && name != r.Session {
			return nil, fmt.Errorf("no saved route to %s/%s; attach from its parent shell", cleanText(host), cleanText(name))
		}
		return r.clone(), nil
	}
	return nil, nil
}

func connectionFromOptions(o options) (recentConnection, error) {
	var record recentConnection
	var cwd string
	dst := &record
	for depth, src := 0, &o; src != nil; depth, src = depth+1, src.routeParent {
		if depth >= maxConnectionHops {
			return recentConnection{}, fmt.Errorf("too many connection hops")
		}
		// A remembered route must not replay the original forwarding rules.
		ssh := transportio.WithoutForwarding(src.ssh)
		directory := src.sshDirectory
		if len(ssh) == 0 {
			directory = ""
		} else if directory == "" {
			if cwd == "" {
				var err error
				cwd, err = os.Getwd()
				if err != nil {
					return recentConnection{}, err
				}
			}
			directory = cwd
		}
		server := src.server
		if server == "" {
			server = "sshore"
		}
		*dst = recentConnection{
			Hosted: src.hosted, Directory: directory, Host: src.host, Session: src.session,
			Server: server, SSH: ssh, Instance: src.instance,
		}
		if src.routeParent != nil {
			dst.Parent, dst.RouteToken, dst.RouteTarget = new(recentConnection), src.routeToken, src.routeTarget
		}
		dst = dst.Parent
	}
	if record.Parent != nil {
		if err := validateRecentConnection(record); err != nil {
			return recentConnection{}, err
		}
	}
	return record, nil
}

func applyConnectionOptions(o *options, r recentConnection) {
	for hop := &r; ; hop = hop.Parent {
		if hop.Hosted {
			o.local, o.hosted = true, true
			o.taskIdentity = "task:" + hop.Instance
		}
		if o.instance == "" && (o.session == "" || o.session == hop.Session) {
			o.instance = hop.Instance
		}
		if !o.serverSet {
			o.server = hop.Server
		}
		if o.portOverrideOnly {
			o.ssh = transportio.AppendWithoutPort(o.ssh, hop.SSH)
			o.sshDirectory = hop.Directory
			o.portOverrideOnly = false
		} else if len(o.ssh) == 0 {
			o.ssh = append([]string(nil), hop.SSH...)
			o.sshDirectory = hop.Directory
		}
		parent := hop.Parent
		if parent == nil {
			return
		}
		o.routeParent = &options{command: "attach", host: parent.Host, session: parent.Session}
		o.routeToken, o.routeTarget = hop.RouteToken, hop.RouteTarget
		o = o.routeParent
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
func (m *daemonMetadata) rememberConnectionLocal(record recentConnection) error {
	if err := validateRecentConnection(record); err != nil {
		return err
	}
	if _, err := m.readRecentConnectionsLocal(); err != nil {
		return err
	}
	return m.connections.promote(*record.clone(), func(previous recentConnection) bool {
		return previous.Host == record.Host
	})
}

// rememberShellRename changes cached labels only after a successful remote rename.
// It does not reorder recent hosts or treat rename as an attachment confirmation.
func rememberShellRename(host, old, name, instance string) error {
	_, err := metadataRPC(metadataRequest{Action: "connection_rename", Host: host, Key: old, Name: name, Instance: instance})
	return err
}
func (a *daemonActivity) rememberShellRenameLocal(host, old, name, instance string) error {
	if validSSHDestination(host) != nil || validSession(old) != nil || validSession(name) != nil {
		return fmt.Errorf("invalid shell rename identity")
	}
	if old == name {
		return nil
	}
	snapshotErr := a.renameShellSnapshotLocked(host, old, name, instance)
	m := &a.metadata
	records, err := m.readRecentConnectionsLocal()
	if err != nil {
		return errors.Join(snapshotErr, err)
	}
	changed := false
	for i := range records {
		if records[i].Host == host && records[i].Session == old && (instance == "" || records[i].Instance == instance) {
			if !changed {
				records = slices.Clone(records)
			}
			records[i].Session = name
			changed = true
		}
	}
	if changed {
		err = m.connections.replace(records)
	}
	return errors.Join(snapshotErr, err)
}

func routeParentOptions(parent options) options {
	parent.command = "attach"
	// The remote entry point accepts immutable task UIDs, so an ancestor rename cannot sever
	// an otherwise live child route. This transport never attaches a terminal.
	if validTaskUID(parent.instance) {
		parent.session = parent.instance
	}
	return parent
}

func openSessionRoute(ctx context.Context, o *options, transports clientTransports) (_ net.Conn, err error) {
	parent := routeParentOptions(*o.routeParent)
	c, err := openClientTransportContext(ctx, &parent, transports)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	reply, err := exchangeRouteRequest(c, message{Type: "spawn_route_connect", Token: o.routeToken, Instance: parent.instance})
	if err != nil {
		return nil, err
	}
	if reply.Type != "route_connected" {
		return nil, fmt.Errorf("spawn route: %s", reply.Error)
	}
	if err := checkPeerVersion(reply.SoftwareVersion, "spawn route"); err != nil {
		return nil, err
	}
	if o.routeTarget {
		reply, err := exchangeRouteRequest(c, message{Type: "shell_connect", Action: o.command, Name: o.session, Instance: o.instance})
		if err != nil {
			return nil, err
		}
		if reply.Type != "setup_ready" {
			return nil, fmt.Errorf("shell connection: %s", reply.Error)
		}
		if err := checkPeerVersion(reply.SoftwareVersion, "routed shell"); err != nil {
			return nil, err
		}
		if !validTaskUID(reply.Instance) || o.instance != "" && o.instance != reply.Instance {
			return nil, fmt.Errorf("routed shell instance changed")
		}
		o.instance = reply.Instance
	}
	o.routeParent.reusable = parent.reusable
	return c, nil
}

func exchangeRouteRequest(c io.ReadWriter, request message) (message, error) {
	request.SoftwareVersion = version
	if err := writeMessage(c, request); err != nil {
		return message{}, err
	}
	return readMessage(c)
}

func confirmSpawnRoute(ctx context.Context, parent options, token string, result error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	parent = routeParentOptions(parent)
	c, err := openClientTransportContext(ctx, &parent, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	request := message{Type: "spawn_route_confirm", Token: token, Instance: parent.instance}
	if result != nil {
		request.Error = cleanText(result.Error())
	}
	reply, err := exchangeRouteRequest(c, request)
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
	o, err := parseOptions(h.Args)
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
		identity, err := proc.LiveIdentity(h.Owner.PID)
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
	switch {
	case o.local && o.command == "attach":
		args = append(args, localEndpointPrefix+o.session)
	case o.local:
		args = append(args, localEndpointPrefix)
	case o.command == "attach":
		args = append(args, remotePathOperand(o.host, o.session))
	default:
		args = append(args, remotePathOperand(o.host, ""))
	}
	if !o.local {
		args = append(args, "--server", o.server)
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
	h := &clientHandoff{Hosted: o.hosted, Args: args, Owner: o.owner, Directory: cwd, Environment: environment, Tree: currentShellTree(), TaskIdentity: o.taskIdentity, Instance: o.instance}
	m, err := localRequest("", identity, message{Type: "handoff", Handoff: h})
	if err != nil {
		return true, err
	}
	if m.Type != "handoff_accepted" {
		return true, fmt.Errorf("remote handoff: %s", m.Error)
	}
	return true, nil
}

type handoffObservation struct {
	conn net.Conn
}

func openHandoffHint(identity, token string) (*handoffObservation, error) {
	c, _, err := openNativeShell(context.Background(), "", identity)
	if err != nil {
		return nil, err
	}
	if err = writeMessage(c, message{Type: "watch_handoff", Token: token}); err == nil {
		var reply message
		reply, err = readMessage(c)
		if err == nil && reply.Type != "watching" {
			err = fmt.Errorf("watch handoff: %s", reply.Error)
		}
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return &handoffObservation{conn: c}, nil
}

func (s *localShell) watchHandoff(c net.Conn, m message) error {
	s.mu.Lock()
	h := s.handoff
	if h == nil || h.token != m.Token || h.attachment == nil {
		s.mu.Unlock()
		return writeMessage(c, message{Type: "error", Error: "handoff unavailable"})
	}
	if s.handoffHints == nil {
		s.handoffHints = make(map[net.Conn]string)
	}
	s.handoffHints[c] = h.host
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.handoffHints, c)
		s.mu.Unlock()
	}()
	if err := writeMessage(c, message{Type: "watching"}); err != nil {
		return err
	}
	_ = c.SetDeadline(time.Time{})
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-s.done:
			_ = c.Close()
		case <-finished:
		}
	}()
	_, err := io.Copy(io.Discard, c)
	return err
}

type localHandoff struct {
	host       string
	token      string
	attachment *attachment // The preparing frontend; nil while awaiting startup.
	reply      chan string // Owned by one phase and cleared on delivery.
}

// The caller holds the shell lock.
func (h *localHandoff) deliver(errorText string) bool {
	if h.reply == nil {
		return false
	}
	h.reply <- errorText
	h.reply = nil
	return true
}

func (s *localShell) requestHandoff(c net.Conn, h *clientHandoff) error {
	deadline := time.Now().Add(2 * time.Minute)
	_ = c.SetDeadline(deadline)
	target, err := h.options()
	if err != nil {
		return writeMessage(c, message{Type: "error", Error: err.Error()})
	}
	host := target.host
	if target.local {
		host = "local"
	}
	s.mu.Lock()
	p := s.active
	if p == nil || s.handoff != nil {
		s.mu.Unlock()
		return writeMessage(c, message{Type: "error", Error: "local frontend unavailable or busy"})
	}
	prepared := make(chan string, 1)
	pending := &localHandoff{token: resourceToken(), attachment: p, reply: prepared, host: host}
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
	case errorText := <-prepared:
		if errorText != "" {
			return writeMessage(c, message{Type: "error", Error: errorText})
		}
	case <-p.Done():
		return writeMessage(c, message{Type: "error", Error: "local frontend disconnected"})
	case <-time.After(4 * time.Second):
		return writeMessage(c, message{Type: "error", Error: "local frontend handoff timed out"})
	}
	started := make(chan string, 1)
	s.mu.Lock()
	pending.attachment, pending.reply = nil, started
	s.mu.Unlock()
	if err := p.controlMessage(message{Type: "handoff_commit", Token: pending.token}); err != nil {
		return err
	}
	select {
	case errorText := <-started:
		if errorText != "" {
			return writeMessage(c, message{Type: "error", Error: errorText})
		}
		return writeMessage(c, message{Type: "handoff_accepted"})
	case <-time.After(time.Until(deadline)):
		return writeMessage(c, message{Type: "error", Error: "attachment startup timed out"})
	}
}

func (s *localShell) finishHandoff(p *attachment, m message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.handoff; h != nil && h.attachment == p && h.token == m.Token {
		h.deliver(m.Error)
	}
}

func (s *localShell) confirmHandoff(c net.Conn, m message) error {
	s.mu.Lock()
	pending := s.handoff
	delivered := pending != nil && pending.attachment == nil && pending.token == m.Token && pending.deliver(m.Error)
	s.mu.Unlock()
	if !delivered {
		return writeMessage(c, message{Type: "error", Error: "handoff expired or not committed"})
	}
	return writeMessage(c, message{Type: "handoff_confirmed"})
}

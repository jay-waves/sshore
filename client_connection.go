package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

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
	treeKey           string
	open              func(context.Context) (net.Conn, error)
	close             func()
	ownsShell         bool
	session, instance string
}

func (ts clientTransports) releaseUnreferenced(keepShells bool) {
	for t, keep := range ts {
		if keep || keepShells && t.ownsShell {
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
		if _, held := ts[o.hold]; held {
			ts[o.hold] = true
		}
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
	t := &clientTransport{open: open, close: closeTask, ownsShell: o.command == "sh" && o.parent == "", session: o.session, instance: o.instance}
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
		warnUnusedRouteForwarding(o)
		return openReusableTransport(ctx, o.reusable)
	}
	o.reusable = nil
	if o.routeParent != nil {
		warnUnusedRouteForwarding(o)
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
		if o.command == "sh" {
			r, err = startLocalSessionContext(ctx, *o)
			if err == nil {
				if o.taskIdentity != "" && o.taskIdentity != r.identity() {
					return nil, fmt.Errorf("local task changed; refresh with sshore list")
				}
				open := func(ctx context.Context) (net.Conn, error) {
					c, err := dialLocalSessionContext(ctx, r, time.Now().Add(5*time.Second))
					if err == nil {
						_ = c.SetDeadline(time.Time{})
					}
					return c, err
				}
				o.taskIdentity, o.session = r.identity(), r.Name
				if o.parent == "" {
					stop, _, err := claimShellOwner(context.Background(), func() (net.Conn, error) { return open(ctx) }, r.UID, "", "")
					if err != nil {
						return nil, err
					}
					return transports.openTaskStream(ctx, o, open, stop)
				}
				c, err = open(ctx)
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
		_ = c.SetDeadline(time.Time{})
		return ownClientTransport(ctx, c, nil), nil
	}
	return openRemoteShellTransport(ctx, o, transports)
}

type sessionSwitch struct {
	from       options
	to         options
	sameDaemon bool
}

func (s *sessionSwitch) Error() string { return "switch session" }
func runClient(o options) (int, error) {
	transports := make(clientTransports)
	defer transports.releaseUnreferenced(false)
	// Keep the frontend alive while an attempt unwinds and restores its parent.
	// Each attempt routes interrupts according to its connection phase.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	var parents []options
	var pending *sessionSwitch
	var currentEndpoint string
	var menu *options
	var current options
	returning := false
	lastExit := 0
	prune := func(current *options) {
		transports.retainRoute(current)
		if pending != nil {
			transports.retainRoute(&pending.from)
		}
		for i := range parents {
			transports.retainRoute(&parents[i])
		}
		transports.releaseUnreferenced(true)
	}
	for {
		prune(&o)
		var ended sessionEnd
		rootTree := !o.local && pending == nil && len(parents) == 0
		code, err := runClientOnce(o, transports, &ended, rootTree, func(attached options) string {
			current, menu = attached, nil
			from := currentEndpoint
			currentEndpoint = shellDisplayEndpoint(current)
			if from == currentEndpoint {
				from = ""
			}
			o.handoffDone = nil
			returning = false
			if pending != nil {
				parents = append(parents, pending.from)
				pending = nil
			}
			prune(&current)
			return from
		})
		if o.handoffDone != nil && pending == nil {
			_ = o.handoffDone(context.Background(), err)
			o.handoffDone = nil
		}
		if next, ok := errors.AsType[*sessionSwitch](err); ok {
			if next.to.recovering {
				returning = true
			} else {
				pending = next
			}
			o = next.to
			continue
		}
		if approval, ok := errors.AsType[*localApproval](err); ok {
			var resume bool
			code, resume, err = executeLocalApproval(approval.request, approval.source)
			if !resume {
				return code, err
			}
			if err != nil {
				termui.Log(os.Stderr, termui.Warning, "[sshore] %s\n", cleanText(err.Error()))
			}
			returning = true
			o = approval.source
			o.command, o.recovering = "attach", true
			continue
		}
		if !returning {
			lastExit = code
		}
		if returning && err != nil {
			problem := fmt.Sprintf("Could not return to %s: %s", cleanText(o.session), cleanText(err.Error()))
			termui.Log(os.Stderr, termui.Warning, "[sshore] %s\n", problem)
			if len(parents) == 0 {
				menu = &o
			}
		}
		var target options
		switch {
		case pending != nil:
			target = pending.from
			pending = nil
			// Release the launcher after its terminal is restored so its error
			// and subsequent shell output reach the user.
			if report := o.handoffDone; report != nil {
				target.handoffDone = func(ctx context.Context, _ error) error { return report(ctx, err) }
			}
			reason := ""
			if err != nil {
				reason = " " + cleanText(err.Error())
			}
			termui.Log(os.Stderr, termui.Warning, "[sshore] Target unavailable; restoring %s.%s\n", cleanText(target.session), reason)
		case len(parents) == 0:
			if o.command == "kill" || o.command == "rename" || err != nil && menu == nil {
				return code, err
			}
			problem := ""
			if err != nil {
				problem, current = err.Error(), *menu
			}
			next, menuErr := runRootConsole(current, problem, ended, "")
			if menuErr != nil {
				return 1, menuErr
			}
			if next == nil {
				return lastExit, nil
			}
			menu = &current
			o, returning = *next, false
			continue
		default:
			last := len(parents) - 1
			target = parents[last]
			parents = slices.Delete(parents, last, last+1)
			if len(ended.retained) > 0 {
				if _, menuErr := runRootConsole(current, "", ended, shellDisplayEndpoint(target)); menuErr != nil {
					return 1, menuErr
				}
			} else if ended.exit {
				sessionNotice(ended.level, ended.title, ended.detail)
			}
			if err != nil && !returning {
				termui.Log(os.Stderr, termui.Warning, "[sshore] %s\n", cleanText(err.Error()))
			}
		}
		returning = true
		o = target
		o.command, o.recovering = "attach", true
	}
}

func resolveRecentAttach(o *options) error {
	if o.command != "attach" || o.local {
		return nil
	}
	return applyRecentConnection(o)
}

func applyRecentConnection(o *options) error {
	reply, err := metadataRPC(metadataRequest{Action: "connection_select", Host: o.host})
	if err != nil {
		return err
	}
	if r := reply.Connection; r != nil {
		o.host = r.Host
		// The history supplies only transport configuration. The authenticated
		// target resolves the supplied selector and confirms the actual instance.
		r.Instance = ""
		applyConnectionOptions(o, *r)
		return nil
	}
	if o.host == "" {
		return usage("no recent connection; use sshore attach HOST:[ID] or sshore sh HOST:")
	}
	return nil
}

func (m *daemonMetadata) selectRecentConnection(host string) (*recentConnection, error) {
	records, err := m.readRecentConnectionsLocal()
	if err != nil {
		return nil, err
	}
	for i := range records {
		if host == "" || host == records[i].Host {
			return records[i].clone(), nil
		}
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
		} else if len(transportio.WithoutForwarding(o.ssh)) == 0 {
			o.ssh = append(o.ssh, hop.SSH...)
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

func warnUnusedRouteForwarding(o *options) {
	args := transportio.WithoutForwarding(o.ssh)
	if len(args) != len(o.ssh) {
		transportio.LogWarning(os.Stderr, transportio.IgnoredForwardingWarning(o.host))
		o.ssh = args
	}
}

func rememberConnection(o options) error {
	if o.session == "" || ownedRemoteRoute(o) {
		return nil
	}
	o = directShellOptions(o)
	if o.local || o.host == "" {
		return nil
	}
	record, err := connectionFromOptions(o)
	if err != nil {
		return err
	}
	_, err = metadataRPC(metadataRequest{Action: "connection_save", Connection: &record})
	return err
}

func ownedRemoteRoute(o options) bool {
	for hop := &o; hop != nil; hop = hop.routeParent {
		if !hop.local && hop.parent != "" || hop.routeParent != nil && !hop.sameHostRoute {
			return true
		}
	}
	return false
}
func directShellOptions(o options) options {
	// A same-host child can be reached directly even after its parent exits.
	hop := &o
	for hop.sameHostRoute && hop.routeParent != nil {
		hop = hop.routeParent
	}
	if hop != &o && hop.routeParent == nil {
		if hop.local {
			o.local, o.hosted, o.taskIdentity = true, true, identityFromUID(o.instance)
		}
		o.routeParent, o.routeToken, o.routeTarget = nil, "", false
		o.server, o.ssh, o.sshDirectory = hop.server, hop.ssh, hop.sshDirectory
	}
	return o
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
	if o.routeToken != "" {
		reply, err := exchangeRouteRequest(c, message{Type: "shell_route_connect", Token: o.routeToken, Instance: parent.instance})
		if err != nil {
			return nil, err
		}
		if reply.Type != "route_connected" {
			return nil, fmt.Errorf("shell route: %s", reply.Error)
		}
		if err := checkPeerVersion(reply.SoftwareVersion, "shell route"); err != nil {
			return nil, err
		}
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

func confirmShellRoute(ctx context.Context, parent options, token, handoffToken string, result error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	parent = routeParentOptions(parent)
	c, err := openClientTransportContext(ctx, &parent, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	request := message{Type: "shell_route_confirm", Token: token, HandoffToken: handoffToken, Instance: parent.instance}
	if result != nil {
		request.Error = cleanText(result.Error())
	}
	reply, err := exchangeRouteRequest(c, request)
	if err != nil {
		return err
	}
	if reply.Type != "route_confirmed" {
		return fmt.Errorf("shell confirmation: %s", reply.Error)
	}
	return nil
}

// Only local authenticated control carries this request; terminal output never does.
type clientHandoff struct {
	Hosted                 bool
	Args                   []string
	Directory              string
	Environment            []string
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
	if (o.command != "sh" && o.command != "attach") || !filepath.IsAbs(h.Directory) {
		return o, fmt.Errorf("invalid remote handoff")
	}
	if h.Hosted && (!o.local || o.command != "attach" || !validTaskUID(h.Instance) || h.TaskIdentity != "task:"+h.Instance) {
		return o, fmt.Errorf("invalid hosted shell handoff")
	}
	o.sshDirectory, o.environment = h.Directory, h.Environment
	o.taskIdentity, o.instance = h.TaskIdentity, h.Instance
	o.hosted = h.Hosted
	return o, nil
}

func handoffLocalClient(o options) (bool, error) {
	identity := os.Getenv("SSHORE_LOCAL_SESSION")
	if identity == "" || (o.command != "sh" && o.command != "attach") || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
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
		args = append(args, localEndpointPrefix+o.session)
	} else {
		args = append(args, transportio.FormatEndpoint(o.host, o.session))
		args = append(args, "--server", o.server)
	}
	for _, flag := range []struct {
		name    string
		enabled bool
	}{{"--no-escape", o.noEscape}, {"--pin", o.pin}, {"--no-login", o.noLogin}, {"--no-profile", o.noProfile}} {
		if flag.enabled {
			args = append(args, flag.name)
		}
	}
	if len(o.ssh) > 0 {
		args = append(append(args, "--"), o.ssh...)
	}
	environment := os.Environ()
	h := &clientHandoff{Hosted: o.hosted, Args: args, Directory: cwd, Environment: environment, TaskIdentity: o.taskIdentity, Instance: o.instance}
	m, err := localRequest("", identity, message{Type: "handoff", Handoff: h})
	if err != nil {
		return true, err
	}
	if m.Type != "handoff_accepted" {
		return true, fmt.Errorf("remote handoff: %s", m.Error)
	}
	return true, nil
}

type localHandoff struct {
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
	if target.command == "attach" && !target.local {
		child, err := s.selectRemoteChild(&shellConfig{Host: target.host, Name: target.session, Instance: target.instance})
		if err != nil {
			return writeMessage(c, message{Type: "error", Error: err.Error()})
		}
		h.Instance = child.uid
	}
	s.mu.Lock()
	p := s.active
	if p == nil || s.handoff != nil {
		s.mu.Unlock()
		return writeMessage(c, message{Type: "error", Error: "local frontend unavailable or busy"})
	}
	prepared := make(chan string, 1)
	pending := &localHandoff{token: resourceToken(), attachment: p, reply: prepared}
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

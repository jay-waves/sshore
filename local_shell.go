package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/tmux"
)

// The token is a local connection capability, never part of public inventory.
type localShellRecord struct {
	UID                                      string `json:",omitempty"`
	Owner                                    processIdentity
	ManagerPID                               int
	Name, Address, Token, Version, Directory string
}

func openNativeShell(ctx context.Context, ref, identity string) (net.Conn, localShellRecord, error) {
	record, err := existingDaemonRecord()
	if err != nil {
		return nil, localShellRecord{}, err
	}
	path := "/tasks/native/open?" + url.Values{"ref": {ref}, "identity": {identity}}.Encode()
	conn, err := dialDaemonStream(ctx, record, time.Now().Add(5*time.Second), path, nil)
	if err != nil {
		return nil, localShellRecord{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var reply localStartup
	err = json.NewDecoder(conn).Decode(&reply)
	if err == nil {
		switch {
		case reply.Error != "":
			err = errors.New(reply.Error)
		case !validTaskUID(reply.Record.UID):
			err = os.ErrNotExist
		}
	}
	if err != nil {
		conn.Close()
		return nil, localShellRecord{}, err
	}
	return conn, reply.Record, nil
}

func readLocalRecord(path, name string) (localShellRecord, error) {
	var r localShellRecord
	if err := privateStatePath(path, false); err != nil {
		return r, err
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	err = json.NewDecoder(io.LimitReader(f, 16*1024)).Decode(&r)
	if err != nil {
		return r, err
	}
	host, port, err := net.SplitHostPort(r.Address)
	n, portErr := strconv.Atoi(port)
	token, tokenErr := hex.DecodeString(r.Token)
	if err != nil || host != "127.0.0.1" || portErr != nil || n < 1 || n > 65535 || tokenErr != nil || len(token) != 32 || (r.Name != name && r.UID != name) || r.UID != "" && !validTaskUID(r.UID) || r.Version == "" {
		return r, fmt.Errorf("invalid local shell record: %s", name)
	}
	return r, nil
}

// Authenticate before sending any shell commands. Each exchange is bounded;
// callers clear the deadline only when they take ownership of the connection.
func dialLocalSessionUntil(r localShellRecord, deadline time.Time) (net.Conn, error) {
	return dialLocalSessionContext(context.Background(), r, deadline)
}

func dialLocalSessionContext(ctx context.Context, r localShellRecord, deadline time.Time) (net.Conn, error) {
	return dialDaemonStream(ctx, r, deadline, "/tasks/"+r.UID+"/open", nil)
}
func dialDaemonStream(ctx context.Context, r localShellRecord, deadline time.Time, path string, input any) (net.Conn, error) {
	if err := checkPeerVersion(r.Version, "local shell"); err != nil {
		return nil, err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, os.ErrDeadlineExceeded
	}
	c, err := (&net.Dialer{Timeout: min(time.Second, remaining)}).DialContext(ctx, "tcp", r.Address)
	if err != nil {
		return nil, fmt.Errorf("local shell %s unavailable: %w", r.Name, err)
	}
	_ = c.SetDeadline(deadline)
	return upgradeDaemonConn(ctx, c, r, path, input)
}

func (r localShellRecord) identity() string {
	if validTaskUID(r.UID) {
		return "task:" + r.UID
	}
	return fmt.Sprintf("local:%x", sha256.Sum256([]byte(r.Token)))
}

// Register the terminal task before publishing readiness. Attachments use the
// account listener and never own the task lifetime.
func serveLocalShell(tasks *daemonTasks, s *localShell, r localShellRecord, ready func(localShellRecord) error, serve func(net.Conn, message) error) error {
	if err := validSession(r.Name); err != nil {
		return err
	}
	if r.Version != version || len(r.Token) != 64 || !filepath.IsAbs(r.Directory) {
		return fmt.Errorf("invalid local startup request")
	}
	s.record = r
	s.tasks = tasks
	stopWatching := superviseSpawnOwner(r.Owner, s.close)
	defer stopWatching()
	if err := tasks.register(r.UID, &nativeShellTask{shell: s, serve: serve}); err != nil {
		return err
	}
	if err := ready(r); err != nil {
		return err
	}
	startupTimer := time.AfterFunc(15*time.Second, func() {
		s.mu.Lock()
		empty := s.terminal == nil
		if empty {
			s.endTerminal()
		}
		s.mu.Unlock()
		if empty {
			s.close()
		}
	})
	defer startupTimer.Stop()
	<-s.done
	return nil
}

type shellTerminal struct {
	mu         sync.Mutex // Protect shell state and attachment ownership.
	terminalMu sync.Mutex // Serialize replay and redraw with live output. Acquire before mu.
	renameMu   sync.Mutex // Keep each shell's history renames in live update order.
	terminal   *tmux.Terminal
	process    processIdentity
	modes      tmux.InputState
	active     *attachment
	exited     bool
}

// change runs with both the task table and shell locked and returns the tree
// identity to persist. The rename lock preserves write order for this shell.
func (s *shellTerminal) renameTask(tasks *daemonTasks, uid, name string, change func() (string, error)) error {
	if err := validAlias(name); err != nil {
		return err
	}
	s.renameMu.Lock()
	defer s.renameMu.Unlock()
	tasks.mu.Lock()
	if err := tasks.checkNameLocked(uid, name, time.Now()); err != nil {
		tasks.mu.Unlock()
		return err
	}
	s.mu.Lock()
	identity, err := change()
	s.mu.Unlock()
	tasks.mu.Unlock()
	if err == nil && identity != "" {
		renameShellTree(identity, name)
	}
	return err
}

var (
	errShellExited   = errors.New("session is exiting")
	errShellAttached = errors.New("session already attached")
)

// Call with mu held.
func (s *shellTerminal) endTerminal() *attachment {
	p := s.active
	s.active, s.exited = nil, true
	return p
}

type shellInput interface {
	tmux.Input
	Stop() error
}

func shellSize(m message) tmux.Size {
	return tmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}
}

// The caller holds mu while publishing the terminal. The exit handler owns
// task-specific state and exit reporting.
func (s *shellTerminal) startTerminal(path string, args, env []string, dir string, m message, exited func(error, int)) error {
	t, err := tmux.Start(tmux.Config{Path: path, Args: args, Env: env, Dir: dir, Size: shellSize(m)})
	if err != nil {
		return err
	}
	pid := t.PID()
	identity, err := spawnProcessIdentity(pid)
	if err != nil {
		t.Close()
		return err
	}
	s.terminal = t
	s.process = processIdentity{PID: pid, Start: identity}
	go s.pumpTerminal(t, exited)
	return nil
}

func (s *shellTerminal) pumpTerminal(t *tmux.Terminal, exited func(error, int)) {
	buf := make([]byte, 8192)
	for {
		n, err := t.Read(buf)
		if n > 0 {
			s.terminalMu.Lock()
			s.modes.Observe(buf[:n])
			// Keep live output ordered without holding the shell lock during I/O.
			s.mu.Lock()
			p := s.active
			s.mu.Unlock()
			if p != nil && p.output(message{Type: "output", Data: buf[:n]}) != nil {
				p.Close()
			}
			s.terminalMu.Unlock()
		}
		if err != nil {
			break
		}
	}
	err := t.Wait()
	code := 0
	if err != nil {
		code = 1
		if e, ok := errors.AsType[interface {
			error
			ExitCode() int
		}](err); ok {
			code = e.ExitCode()
		}
	}
	exited(err, code)
}

// Caller holds both host locks. The attachment acquires ownership only after
// startup, terminal setup and the ready handshake have succeeded.
func (s *shellTerminal) attachTerminal(p *attachment, m message, start func(message) error, ready func() error) error {
	if s.exited {
		return errShellExited
	}
	if s.active != nil {
		return errShellAttached
	}
	initial := s.terminal == nil
	if initial {
		if err := start(m); err != nil {
			return err
		}
	} else if m.NoLogin || m.NoProfile {
		return fmt.Errorf("startup options require a new managed shell")
	}
	if err := s.terminal.Resize(shellSize(m)); err != nil {
		return err
	}
	lease := tmux.NewInputLease(s.terminal)
	err := ready()
	if err == nil {
		if data := s.modes.Replay(); len(data) > 0 {
			err = p.output(message{Type: "output", Data: data})
		}
	}
	if err != nil {
		lease.Stop()
		return err
	}
	p.input = lease
	s.active = p
	if !initial {
		tmux.RequestRedraw(s.terminal, s.process.PID)
	}
	return nil
}

// Hold mu through Stop so no successor can acquire the terminal's writer.
func (s *shellTerminal) detachTerminal(p *attachment) (bool, error) {
	active := s.active == p
	if p.input == nil || !active && !s.exited {
		return false, nil
	}
	input := p.input
	p.input = nil
	err := input.Stop()
	// An exited shell has no successor, but its input worker must still join.
	if !active {
		return false, nil
	}
	if err != nil {
		s.endTerminal()
		return false, err
	}
	s.active = nil
	return true, nil
}

// Once attached, only this loop reads control frames. The host records release
// under the shell lock, and may return cleanup that needs to run outside it.
func (s *shellTerminal) serveAttachment(p *attachment, control io.Reader, resizeAck bool, onRelease func(error) (string, func()), extra func(message) error) error {
	release := func() (string, error) {
		s.mu.Lock()
		detached, err := s.detachTerminal(p)
		var name string
		var cleanup func()
		if detached || err != nil {
			name, cleanup = onRelease(err)
		}
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return name, err
	}
	released := false
	inputDone := make(chan struct{})
	defer func() {
		p.Close()
		if !released {
			_, _ = release()
		}
		<-inputDone
	}()
	input := p.input
	go func() {
		defer close(inputDone)
		p.ReadInput(input, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.active == p
		})
	}()
	for {
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		switch m.Type {
		case "detach":
			released = true
			name, err := release()
			if err == nil && name != "" {
				return p.FinishDetach(name)
			}
			return err
		case "resize":
			s.mu.Lock()
			if s.active == p {
				err = s.terminal.Resize(shellSize(m))
			}
			s.mu.Unlock()
			if err != nil {
				return fmt.Errorf("resize terminal: %w", err)
			}
			if resizeAck {
				err = p.controlMessage(message{Type: "resized", Cols: m.Cols, Rows: m.Rows})
			}
		case "redraw":
			s.terminalMu.Lock()
			s.mu.Lock()
			if s.active == p {
				tmux.RequestRedraw(s.terminal, s.process.PID)
			}
			s.mu.Unlock()
			s.terminalMu.Unlock()
			err = p.controlMessage(message{Type: "redrawn"})
		default:
			err = extra(m)
		}
		if err != nil {
			return err
		}
	}
}

type localShell struct {
	shellTerminal
	environment []string
	tasks       *daemonTasks
	record      localShellRecord
	done        chan struct{}
	once        sync.Once
	handoff     *localHandoff
}

func (s *localShell) view() localShellView {
	s.mu.Lock()
	defer s.mu.Unlock()
	temporary := s.record.Owner.PID != 0
	view := localShellView{
		Lifetime: shellLifetime(temporary, false), Temporary: temporary, identity: s.record.identity(),
		Name: s.record.Name, Directory: s.record.Directory, Version: s.record.Version, PID: s.process.PID,
	}
	switch {
	case s.exited:
		view.State = "unavailable"
	case s.active != nil:
		view.State = "attached"
	case view.PID == 0:
		view.State = "starting"
	default:
		view.State = "detached"
	}
	return view
}

func (s *localShell) kill() error {
	s.mu.Lock()
	process := s.process
	s.mu.Unlock()
	if process.PID != 0 {
		if err := stopSpawnTerminal(process.PID, process.Start); err != nil {
			return err
		}
	}
	s.close()
	return nil
}

func (s *localShell) close() {
	s.once.Do(func() {
		s.mu.Lock()
		terminal, owner := s.terminal, s.endTerminal()
		temporary, process := s.record.Owner.PID != 0, s.process
		identity, name := s.record.identity(), s.record.Name
		s.mu.Unlock()
		if owner != nil {
			owner.Close()
		}
		if terminal != nil {
			if temporary {
				_ = stopSpawnTerminal(process.PID, process.Start)
			}
			terminal.Close()
		}
		saveShellTree(identity, name, nil, "exited")
		close(s.done)
	})
}

// Caller holds mu while starting and publishing the terminal. One reader owns
// terminal output for its lifetime, including periods without an attachment.
func (s *localShell) start(m message) error {
	source := s.environment
	if source == nil {
		source = os.Environ()
	}
	path := localShellPath(source)
	args, err := tmux.StartupArgs(path, m.NoLogin, m.NoProfile)
	if err != nil {
		return err
	}
	env := tmux.StartupEnv(path, localShellEnvironment(source, m.Term, s.record.identity()), m.NoProfile)
	return s.startTerminal(path, args, env, s.record.Directory, m, func(err error, code int) {
		reason := "shell exited"
		if err != nil {
			reason = err.Error()
		}
		s.mu.Lock()
		p := s.endTerminal()
		s.mu.Unlock()
		if p != nil {
			p.FinishExit(code, reason)
		}
		s.close()
	})
}

func localShellEnvironment(source []string, term, identity string) []string {
	env := make([]string, 0, len(source)+2)
	for _, item := range source {
		key, _, _ := strings.Cut(item, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		switch key {
		case "SSHORE_SOCKET", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "SSHORE_TTY", "SSHORE_LOCAL_SESSION", "SSHORE_TREE_ID", "TERM":
			continue
		}
		env = append(env, item)
	}
	env = append(env, "TERM="+term, "SSHORE_LOCAL_SESSION="+identity, "SSHORE_TREE_ID="+identity)
	return env
}

func (s *localShell) rename(old, name string) error {
	return s.renameTask(s.tasks, s.record.UID, name, func() (string, error) {
		if s.exited {
			return "", fmt.Errorf("local shell has ended")
		}
		if old != "" && s.record.Name != old {
			return "", fmt.Errorf("local shell changed; refresh with sshore list")
		}
		if s.record.Name == name {
			return "", nil
		}
		s.record.Name = name
		return s.record.identity(), nil
	})
}

func (s *localShell) targets(request message) message {
	s.mu.Lock()
	name := s.record.Name
	s.mu.Unlock()
	return localSessionTargets(name, request)
}

func localSessionTargets(name string, request message) message {
	reply := message{Type: "session_targets", Action: request.Action, Name: name}
	rows, err := localShells(false)
	if err != nil {
		reply.Error = err.Error()
		return reply
	}
	for _, row := range rows {
		if row.State != "detached" {
			continue
		}
		if request.Action == "select" && row.Name != request.Name {
			continue
		}
		reply.Targets = append(reply.Targets, sessionTarget{Name: row.Name, Instance: canonicalTaskUID(row.identity), Hosted: row.hosted})
	}
	if request.Action == "select" && len(reply.Targets) != 1 {
		reply.Error = fmt.Sprintf("local shell %s unavailable", cleanText(request.Name))
	}
	if len(reply.Targets) > 128 {
		reply.Targets = nil
		reply.Error = "too many local shells; use sshore list --local"
	}
	return reply
}

func detachLocalShell(identity string) error {
	m, err := localRequest("", identity, message{Type: "detach"})
	if err != nil {
		return err
	}
	if m.Type != "detach_requested" && m.Type != "detached" {
		return fmt.Errorf("local detach unconfirmed")
	}
	return nil
}

func runLocalShell(tasks *daemonTasks, record localShellRecord, env []string, ready func(localShellRecord) error) error {
	s := &localShell{done: make(chan struct{}), environment: env}
	defer s.close()
	return serveLocalShell(tasks, s, record, ready, func(c net.Conn, m message) error {
		switch m.Type {
		case "handoff_confirm":
			return s.confirmHandoff(c, m)
		case "handoff":
			return s.requestHandoff(c, m.Handoff)
		case "status":
			view := s.view()
			return writeMessage(c, message{
				Type: "session", Temporary: view.Temporary, Name: view.Name, SoftwareVersion: view.Version,
				Attached: view.State == "attached", PID: view.PID,
			})
		case "rename":
			err := s.rename(m.Action, m.Name)
			reply := message{Type: "renamed", Name: m.Name}
			if err != nil {
				reply.Type = "error"
				reply.Error = err.Error()
			}
			return writeMessage(c, reply)
		case "detach":
			s.mu.Lock()
			p := s.active
			s.mu.Unlock()
			if p == nil {
				return writeMessage(c, message{Type: "detached"})
			}
			if err := p.controlMessage(message{Type: "detach_requested"}); err != nil {
				return err
			}
			return writeMessage(c, message{Type: "detach_requested"})
		case "kill":
			if err := s.kill(); err != nil {
				return writeMessage(c, message{Type: "error", Error: err.Error()})
			}
			return writeMessage(c, message{Type: "killed"})
		case "mux":
			if m.SoftwareVersion != version {
				return fmt.Errorf("local version mismatch")
			}
			if err := writeMessage(c, message{Type: "mux_ready", SoftwareVersion: version}); err != nil {
				return err
			}
			return s.attach(c)
		default:
			return fmt.Errorf("unexpected local request")
		}
	})
}

func (s *localShell) attach(c net.Conn) error {
	mx, control, terminal, m, err := acceptMux(c)
	if err != nil {
		return err
	}
	if err := validateAttach(m); err != nil {
		mx.Close()
		return err
	}
	p := newAttachment(mx, control, terminal)
	s.terminalMu.Lock()
	s.mu.Lock()
	initial := s.terminal == nil
	err = s.attachTerminal(p, m, s.start, func() error {
		saveShellTreeWithProcess(s.record.identity(), s.record.Name, m.Tree, "attached", describeSpawnOwner(s.record.Owner), shellLifetime(s.record.Owner.PID != 0, false), m.SourceHost, m.SourceMachineID)
		return p.controlMessage(message{Type: "ready", Tree: shellTreeFor(s.record.identity()), Temporary: s.record.Owner.PID != 0, Name: s.record.Name, PID: s.process.PID})
	})
	if err != nil && initial && !errors.Is(err, errShellAttached) {
		s.endTerminal()
	}
	s.mu.Unlock()
	s.terminalMu.Unlock()
	if err != nil {
		if errors.Is(err, errShellExited) || errors.Is(err, errShellAttached) {
			err = fmt.Errorf("local shell unavailable or already attached")
		} else if initial {
			s.close()
		}
		_ = p.controlMessage(message{Type: "fatal", Error: err.Error()})
		p.Close()
		return err
	}
	return s.serveAttachment(p, control, false, func(err error) (string, func()) {
		if err != nil {
			return "", s.close
		}
		saveShellTree(s.record.identity(), s.record.Name, nil, "detached")
		return s.record.Name, nil
	}, func(m message) error {
		switch m.Type {
		case "handoff_result":
			s.finishHandoff(p, m)
		case "spawn_child":
			s.mu.Lock()
			h := &clientHandoff{Args: []string{"spawn", localEndpointPrefix}, Owner: s.process, Directory: s.record.Directory, Environment: append([]string(nil), s.environment...), Tree: shellTreeFor(s.record.identity())}
			s.mu.Unlock()
			go serveShortcutSpawn(p, s.done, func(c net.Conn) { _ = s.requestHandoff(c, h) })
		case "session_targets":
			return p.controlMessage(s.targets(m))
		default:
			return fmt.Errorf("unexpected local control request")
		}
		return nil
	})
}

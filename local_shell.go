package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jay-waves/sshore/internal/tmux"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The token is a local connection capability, never part of public inventory.
type localShellRecord struct {
	UID                                      string `json:",omitempty"`
	Owner                                    processIdentity
	ManagerPID                               int
	Name, Address, Token, Version, Directory string
}

func readLocalSession(name string) (localShellRecord, error) {
	records, err := nativeShellRecords()
	if err != nil {
		return localShellRecord{}, err
	}
	var found *localShellRecord
	for _, r := range records {
		if r.UID == name || isShortTaskID(name) && strings.HasPrefix(r.UID, strings.ToLower(name)) {
			if found != nil {
				return localShellRecord{}, fmt.Errorf("ambiguous task ID")
			}
			copy := r
			found = &copy
		}
	}
	if found != nil {
		return *found, nil
	}
	for _, r := range records {
		if r.Name == name {
			return r, nil
		}
	}
	return localShellRecord{}, os.ErrNotExist
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
func dialLocalSession(r localShellRecord) (net.Conn, error) {
	return dialLocalSessionUntil(r, time.Now().Add(5*time.Second))
}

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
			s.lifecycle.End()
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
	mu         sync.Mutex
	terminalMu sync.Mutex // Serialize replay and redraw with live output. Acquire before mu.
	terminal   *tmux.Terminal
	process    processIdentity
	modes      tmux.InputState
	lifecycle  tmux.Lifecycle[*attachment]
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
			p := s.lifecycle.Active()
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
		if e, ok := errors.AsType[*exec.ExitError](err); ok {
			code = e.ExitCode()
		}
	}
	exited(err, code)
}

// Caller holds both host locks. The attachment acquires ownership only after
// startup, terminal setup and the ready handshake have succeeded.
func (s *shellTerminal) attachTerminal(p *attachment, m message, start func(message) error, ready func() error) (shellInput, error) {
	var input shellInput
	initial := s.terminal == nil
	err := s.lifecycle.Attach(p, func() error {
		if !initial && (m.NoLogin || m.NoProfile) {
			return fmt.Errorf("startup options require a new managed shell")
		}
		if initial {
			if err := start(m); err != nil {
				return err
			}
		}
		if err := s.terminal.Resize(shellSize(m)); err != nil {
			return err
		}
		lease, err := tmux.NewTerminalInput(s.terminal)
		if err != nil {
			return err
		}
		if err = ready(); err == nil {
			if data := s.modes.Replay(); len(data) > 0 {
				err = p.output(message{Type: "output", Data: data})
			}
		}
		if err != nil {
			lease.Stop()
			return err
		}
		input = lease
		return nil
	})
	if err == nil && !initial && !p.Muted() {
		tmux.RequestRedraw(s.terminal, s.process.PID)
	}
	return input, err
}

// Hold mu through Stop so no successor can acquire the terminal's writer.
func (s *shellTerminal) detachTerminal(p *attachment, input shellInput) (bool, error) {
	if s.lifecycle.Active() != p {
		// An exited shell has no successor, but the input worker still needs joining.
		if s.lifecycle.Exited() {
			_ = input.Stop()
		}
		return false, nil
	}
	if err := input.Stop(); err != nil {
		s.lifecycle.EndIfAttached(p)
		return false, err
	}
	return s.lifecycle.Detach(p), nil
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
		terminal, owner := s.terminal, s.lifecycle.End()
		temporary, process := s.record.Owner.PID != 0, s.process
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
		saveShellTree(s.record.identity(), s.record.Name, nil, "exited")
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
		p := s.lifecycle.End()
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
		case "SSHORE_SOCKET", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "SSHORE_LOCAL_SESSION", "SSHORE_TREE_ID", "TERM":
			continue
		}
		env = append(env, item)
	}
	env = append(env, "TERM="+term, "SSHORE_LOCAL_SESSION="+identity, "SSHORE_TREE_ID="+identity)
	return env
}

func (s *localShell) rename(name string) error {
	if err := validAlias(name); err != nil {
		return err
	}
	s.tasks.mu.Lock()
	defer s.tasks.mu.Unlock()
	for id, task := range s.tasks.records {
		if id != s.record.UID && task.taskName() == name {
			return fmt.Errorf("%s: %w", name, errObjectNameInUse)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle.Exited() {
		return fmt.Errorf("local shell has ended")
	}
	s.record.Name = name
	renameShellTree(s.record.identity(), name)
	return nil
}

func (s *localShell) targets(request message) message {
	s.mu.Lock()
	name := s.record.Name
	s.mu.Unlock()
	return localSessionTargets(name, request)
}

func localSessionTargets(name string, request message) message {
	reply := message{Type: "session_targets", Action: request.Action, Name: name}
	if request.Action == "new" {
		reservation, err := chooseTaskIdentity("shell", "")
		if err != nil {
			reply.Error = err.Error()
			return reply
		}
		reply.Names = []string{reservation.name}

		return reply
	}
	rows, err := localShells()
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
		reply.Names = append(reply.Names, row.Name)
	}
	if request.Action == "select" && len(reply.Names) != 1 {
		reply.Error = fmt.Sprintf("local shell %s unavailable", cleanText(request.Name))
	}
	if len(reply.Names) > 128 {
		reply.Names = nil
		reply.Error = "too many local shells; use sshore list --local"
	}
	return reply
}

func detachLocalShell(identity string) error {
	rows, err := localShells()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.identity != identity {
			continue
		}
		m, err := localRequest(row.Name, message{Type: "detach"}, identity)
		if err != nil {
			return err
		}
		if m.Type != "detach_requested" && m.Type != "detached" {
			return fmt.Errorf("local detach unconfirmed")
		}
		return nil
	}
	return fmt.Errorf("local shell is unavailable")
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
			s.mu.Lock()
			reply := message{Type: "session", Temporary: s.record.Owner.PID != 0, Name: s.record.Name, SoftwareVersion: version, Attached: s.lifecycle.Active() != nil}
			if s.terminal != nil {
				reply.PID = s.terminal.PID()
			}
			s.mu.Unlock()
			return writeMessage(c, reply)
		case "rename":
			err := s.rename(m.Name)
			reply := message{Type: "renamed", Name: m.Name}
			if err != nil {
				reply.Type = "error"
				reply.Error = err.Error()
			}
			return writeMessage(c, reply)
		case "detach":
			s.mu.Lock()
			p := s.lifecycle.Active()
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
	defer mx.Close()
	p := newAttachment(mx, control, terminal)
	if err := validateAttach(m); err != nil {
		return err
	}
	s.terminalMu.Lock()
	s.mu.Lock()
	initial := s.terminal == nil
	lease, err := s.attachTerminal(p, m, s.start, func() error {
		saveShellTreeWithProcess(s.record.identity(), s.record.Name, m.Tree, "attached", describeSpawnOwner(s.record.Owner), shellLifetime(s.record.Owner.PID != 0, false), m.SourceHost, m.SourceMachineID)
		return p.controlMessage(message{Type: "ready", Tree: shellTreeFor(s.record.identity()), Temporary: s.record.Owner.PID != 0, Name: s.record.Name, PID: s.process.PID})
	})
	if err != nil && initial && !errors.Is(err, tmux.ErrAttached) {
		s.lifecycle.End()
	}
	t := s.terminal
	s.mu.Unlock()
	s.terminalMu.Unlock()
	if err != nil {
		if errors.Is(err, tmux.ErrExited) || errors.Is(err, tmux.ErrAttached) {
			err = fmt.Errorf("local shell unavailable or already attached")
		} else if initial {
			s.close()
		}
		_ = p.controlMessage(message{Type: "fatal", Error: err.Error()})
		return err
	}
	release := sync.OnceValues(func() (string, error) {
		s.mu.Lock()
		detached, err := s.detachTerminal(p, lease)
		if detached {
			saveShellTree(s.record.identity(), s.record.Name, nil, "detached")
		}
		name := s.record.Name
		s.mu.Unlock()
		if err != nil {
			s.close()
		}
		return name, err
	})
	defer release()
	go p.readInput(lease, &s.lifecycle)

	for {
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		switch m.Type {
		case "handoff_result":
			s.finishHandoff(p, m)
		case "detach":
			name, err := release()
			if err != nil {
				return err
			}
			p.FinishDetach(name)
			return nil
		case "spawn_child":
			s.mu.Lock()
			h := &clientHandoff{Args: []string{"spawn", "--local"}, Owner: s.process, Directory: s.record.Directory, Environment: append([]string(nil), s.environment...), Tree: shellTreeFor(s.record.identity())}
			s.mu.Unlock()
			go serveShortcutSpawn(p, s.done, func(c net.Conn) { _ = s.requestHandoff(c, h) })
		case "session_targets":
			if err = p.controlMessage(s.targets(m)); err != nil {
				return err
			}
		case "resize":
			if err = t.Resize(tmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}); err != nil {
				return err
			}
		case "redraw":
			redrawLocalTerminal(t)
			if err = p.controlMessage(message{Type: "redrawn"}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected local control request")
		}
	}
}

func redrawLocalTerminal(t *tmux.Terminal) { tmux.RequestRedraw(t, t.PID()) }

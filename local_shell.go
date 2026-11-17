package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/jay-waves/sshore/internal/tmux"
	"io"
	"net"
	"os"
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
	Owner                                    spawnOwner
	ManagerPID                               int
	Name, Address, Token, Version, Directory string
}

func localSessionPath(name string) (string, error) {
	if err := validSession(name); err != nil {
		return "", err
	}
	dir, err := stateDir("local-shells")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

func readLocalSession(name string) (localShellRecord, error) {
	var r localShellRecord
	path, err := localSessionPath(name)
	if err != nil {
		return r, err
	}
	r, err = readLocalRecord(path, name)
	if err == nil || !os.IsNotExist(err) {
		return r, err
	}
	dir, err := stateDir("local-shells")
	if err != nil {
		return r, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return r, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		candidate, err := readLocalRecord(filepath.Join(dir, entry.Name()), strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return r, err
		}
		if candidate.Name == name || validTaskUID(candidate.UID) && candidate.UID[:6] == name {
			return candidate, nil
		}
	}
	return r, os.ErrNotExist
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
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetDeadline(deadline)
	if err = writeMessage(c, message{Type: "local_auth", Token: r.Token, SoftwareVersion: version}); err == nil {
		var m message
		m, err = readMessage(c)
		if err == nil {
			err = checkPeerVersion(m.SoftwareVersion, "local shell")
			if err == nil && m.Type != "local_ready" {
				err = fmt.Errorf("local shell authentication failed")
			}
		}
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (r localShellRecord) identity() string {
	if validTaskUID(r.UID) {
		return "task:" + r.UID
	}
	return fmt.Sprintf("local:%x", sha256.Sum256([]byte(r.Token)))
}

// The listener owns no terminal until the first authenticated attachment.
// serve runs synchronously for each connection; shutdown joins all handlers.
func serveLocalShell(s *localShell, r localShellRecord, ready func(localShellRecord) error, serve func(net.Conn, message) error, done <-chan struct{}) (result error) {
	if err := validSession(r.Name); err != nil {
		return err
	}
	if r.Version != version || len(r.Token) != 64 || !filepath.IsAbs(r.Directory) {
		return fmt.Errorf("invalid local startup request")
	}
	owner, err := lockObjectName(r.Name)
	if err != nil {
		return err
	}
	s.nameLock = owner
	defer func() { s.nameLock.Close() }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	r.Address = listener.Addr().String()
	path, err := localSessionPath(r.storageName())
	if err != nil {
		return err
	}
	if err = writeStateJSON(path, r); err != nil {
		return err
	}
	s.record = r
	go watchSpawnOwner(r.Owner, s.done, s.close)
	defer func() {
		path, err := localSessionPath(s.record.storageName())
		if err == nil {
			os.Remove(path)
		}
	}()
	if err = ready(r); err != nil {
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
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	var workers sync.WaitGroup
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-done:
			_ = listener.Close()
		case <-stopped:
		}
	}()
	defer func() {
		mu.Lock()
		for c := range connections {
			_ = c.Close()
		}
		mu.Unlock()
		workers.Wait()
	}()
	for {
		c, err := listener.Accept()
		if err != nil {
			select {
			case <-done:
				return nil
			default:
				return err
			}
		}
		mu.Lock()
		if len(connections) >= 32 {
			mu.Unlock()
			c.Close()
			continue
		}
		connections[c] = true
		mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { c.Close(); mu.Lock(); delete(connections, c); mu.Unlock() }()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			m, err := readMessage(c)
			if err != nil || m.Type != "local_auth" || subtle.ConstantTimeCompare([]byte(m.Token), []byte(r.Token)) != 1 {
				return
			}
			if m.SoftwareVersion != version {
				_ = writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: "local shell version mismatch"})
				return
			}
			if writeMessage(c, message{Type: "local_ready", SoftwareVersion: version}) != nil {
				return
			}
			m, err = readMessage(c)
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Time{})
			_ = serve(c, m)
		}()
	}
}

func (r localShellRecord) storageName() string {
	if validTaskUID(r.UID) {
		return r.UID
	}
	return r.Name
}

type localShell struct {
	processIdentity string
	environment     []string
	nameLock        *os.File
	mu              sync.Mutex
	record          localShellRecord
	terminal        *tmux.Terminal
	lifecycle       tmux.Lifecycle[*attachment]
	modes           tmux.InputState
	done            chan struct{}
	once            sync.Once
	handoff         *localHandoff
}

func (s *localShell) close() {
	s.once.Do(func() {
		s.mu.Lock()
		terminal, owner := s.terminal, s.lifecycle.End()
		temporary, identity := s.record.Owner.PID != 0, s.processIdentity
		s.mu.Unlock()
		if owner != nil {
			owner.Mux().Close()
		}
		if terminal != nil {
			if temporary {
				_ = stopSpawnTerminal(terminal.PID(), identity)
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
	t, err := tmux.Start(tmux.Config{Path: path, Args: args, Env: env, Dir: s.record.Directory, Size: tmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}})
	if err != nil {
		return err
	}
	if s.record.Owner.PID != 0 {
		identity, err := spawnProcessIdentity(t.PID())
		if err != nil {
			t.Close()
			return err
		}
		s.processIdentity = identity
	}
	s.terminal = t
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := t.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.modes.Observe(buf[:n])
				if p := s.lifecycle.Active(); p != nil {
					if p.output(message{Type: "output", Data: buf[:n]}) != nil {
						p.Mux().Close()
					}
				}
				s.mu.Unlock()
			}
			if err != nil {
				break
			}
		}
		err := t.Wait()
		code := 0
		if err != nil {
			code = 1
		}
		s.mu.Lock()
		p := s.lifecycle.Active()
		s.mu.Unlock()
		if p != nil {
			p.FinishExit(code)
		}
		s.close()
	}()
	return nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle.Exited() {
		return fmt.Errorf("local shell has ended")
	}
	if s.record.Name == name {
		return nil
	}
	reservation, err := reserveResourceName("shell", name)
	if err != nil {
		return err
	}
	defer reservation.Close()
	next := s.record
	next.Name = name
	path, err := localSessionPath(next.storageName())
	if err != nil {
		return err
	}
	if err = writeStateJSON(path, next); err != nil {
		return err
	}
	oldPath, _ := localSessionPath(s.record.storageName())
	if oldPath != path {
		if err = os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
			os.Remove(path)
			return err
		}
	}
	s.record = next
	renameShellTree(s.record.identity(), name)
	s.nameLock.Close()
	s.nameLock = reservation.owner
	reservation.owner = nil
	return nil
}

func (s *localShell) targets(request message) message {
	s.mu.Lock()
	name := s.record.Name
	s.mu.Unlock()
	reply := message{Type: "session_targets", Action: request.Action, Name: name}
	if request.Action == "new" {
		reservation, err := reserveResourceName("shell", "")
		if err != nil {
			reply.Error = err.Error()
			return reply
		}
		reply.Names = []string{reservation.name}
		reservation.Close()
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

func runLocalShell(record localShellRecord, env []string, ready func(localShellRecord) error) error {
	s := &localShell{done: make(chan struct{}), environment: env}
	defer s.close()
	return serveLocalShell(s, record, ready, func(c net.Conn, m message) error {
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
		case "cancel":
			_ = writeMessage(c, message{Type: "cancelled"})
			s.close()
			return nil
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
	}, s.done)
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
	s.mu.Lock()
	if s.lifecycle.Active() != nil || s.lifecycle.Exited() {
		s.mu.Unlock()
		_ = p.controlMessage(message{Type: "fatal", Error: "local shell unavailable or already attached"})
		return fmt.Errorf("local shell unavailable")
	}
	initial := s.terminal == nil
	if (m.NoLogin || m.NoProfile) && !initial {
		s.mu.Unlock()
		_ = p.controlMessage(message{Type: "fatal", Error: "startup options require a new shell"})
		return fmt.Errorf("startup options require a new shell")
	}
	if initial {
		err = s.start(m)
	}
	if err == nil {
		err = s.terminal.Resize(tmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)})
	}
	if err == nil {
		if initial {
			saveShellTree(s.record.identity(), s.record.Name, m.Tree, "attached", shellLifetime(s.record.Owner.PID != 0, false), m.SourceHost, m.SourceMachineID)
		} else {
			saveShellTree(s.record.identity(), s.record.Name, m.Tree, "attached", shellLifetime(s.record.Owner.PID != 0, false), m.SourceHost, m.SourceMachineID)
		}
		err = p.controlMessage(message{Type: "ready", Tree: shellTreeFor(s.record.identity()), Temporary: s.record.Owner.PID != 0, Name: s.record.Name, PID: s.terminal.PID()})
	}
	if err == nil {
		err = p.output(message{Type: "output", Data: s.modes.Replay()})
	}
	if err != nil {
		s.mu.Unlock()
		_ = p.controlMessage(message{Type: "fatal", Error: err.Error()})
		if initial {
			s.close()
		}
		return err
	}
	t := s.terminal
	lease, err := tmux.NewTerminalInput(t)
	if err != nil {
		s.mu.Unlock()
		if initial {
			s.close()
		}
		return err
	}
	if err = s.lifecycle.Attach(p, func() error { return nil }); err != nil {
		s.mu.Unlock()
		lease.Stop()
		return err
	}
	saveShellTree(s.record.identity(), s.record.Name, m.Tree, "attached", shellLifetime(s.record.Owner.PID != 0, false), m.SourceHost, m.SourceMachineID)
	p.SetInput(lease)
	s.mu.Unlock()
	// Losing a local client releases attachment ownership, not the daemon-owned
	// terminal. Stop its writer before another client can attach.
	defer func() {
		lease.Stop()
		s.mu.Lock()
		if s.lifecycle.Detach(p) {
			saveShellTree(s.record.identity(), s.record.Name, nil, "detached")
		}
		s.mu.Unlock()
	}()
	go p.readInput(&s.lifecycle)
	if !initial {
		// Output ownership must be installed before requesting a repaint.
		redrawLocalTerminal(t)
	}

	for {
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		switch m.Type {
		case "handoff_result":
			s.finishHandoff(p, m)
		case "detach":
			s.mu.Lock()
			if err := lease.Stop(); err != nil {
				s.mu.Unlock()
				return err
			}
			if s.lifecycle.Detach(p) {
				saveShellTree(s.record.identity(), s.record.Name, nil, "detached")
			}
			name := s.record.Name
			s.mu.Unlock()
			tmux.FinishDetach(control, func() error { return p.controlMessage(message{Type: "detached", Name: name}) }, func() { mx.Close() })
			return nil
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

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

type localShell struct {
	shellTerminal
	environment  []string
	tasks        *daemonTasks
	record       localShellRecord
	done         chan struct{}
	once         sync.Once
	handoff      *localHandoff
	handoffHints map[net.Conn]string
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
	if len(s.handoffHints) != 0 {
		hosts := slices.Sorted(maps.Values(s.handoffHints))
		view.ConnectedHost = strings.Join(slices.Compact(hosts), ", ")
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
	env := localShellEnvironment(source, m.Term, s.record.identity())
	return s.startShell(path, env, s.record.Directory, m, func(err error, code int) {
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
		reply.Error = "too many local shells; use sshore list here:"
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
		case "watch_handoff":
			return s.watchHandoff(c, m)
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
		case "terminal_open":
			return s.endpoints.serve(c, m)
		case "attach_control":
			if m.SoftwareVersion != version {
				return fmt.Errorf("local version mismatch")
			}
			if err := writeMessage(c, message{Type: "attach_control_ready", SoftwareVersion: version}); err != nil {
				return err
			}
			return s.attach(c)
		default:
			return fmt.Errorf("unexpected local request")
		}
	})
}

func (s *localShell) attach(c net.Conn) error {
	terminal, m, err := acceptAttachment(c, &s.endpoints)
	if err != nil {
		return err
	}
	if err := validateAttach(m); err != nil {
		terminal.Close()
		return err
	}
	p := newAttachment(c, terminal)
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
	return s.serveAttachment(p, c, false, func(err error) (string, func()) {
		if err != nil {
			return "", s.close
		}
		saveShellTree(s.record.identity(), s.record.Name, nil, "detached")
		return s.record.Name, nil
	}, func(m message) error {
		switch m.Type {
		case "handoff_result":
			s.finishHandoff(p, m)
		case "spawn_child", "launch_child":
			command := strings.TrimSuffix(m.Type, "_child")
			s.mu.Lock()
			h := &clientHandoff{Args: []string{command, localEndpointPrefix}, Owner: s.process, Directory: s.record.Directory, Environment: append([]string(nil), s.environment...), Tree: shellTreeFor(s.record.identity())}
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

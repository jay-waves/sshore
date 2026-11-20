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
	shellTask
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
	view.State = s.state()
	if len(s.handoffHints) != 0 {
		hosts := slices.Sorted(maps.Values(s.handoffHints))
		view.ConnectedHost = strings.Join(slices.Compact(hosts), ", ")
	}
	return view
}

func (s *localShell) start(m message) error {
	source := s.shellEnvironment()
	return s.startManagedShell(localShellPath(source), localShellEnvironment(source, m.Term, s.record.identity()), m)
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
	if runtime.GOOS == "darwin" {
		return fmt.Errorf("local shells are unsupported on macOS")
	}
	s := &localShell{shellTask: shellTask{done: make(chan struct{}), environment: env}}
	defer s.close()
	return serveLocalShell(tasks, s, record, ready, func(c net.Conn, m message) error {
		switch m.Type {
		case "watch_handoff":
			return s.watchHandoff(c, m)
		case "handoff_confirm":
			return s.confirmHandoff(c, m)
		case "handoff":
			return s.requestHandoff(c, m.Handoff)

		default:
			handled, err := s.serveCommand(c, m, s.attach, s.status)
			if handled {
				return err
			}
			return fmt.Errorf("unexpected local request")
		}
	})
}

func (s *localShell) status(_ string) message {
	view := s.view()
	return message{Type: "session", Temporary: view.Temporary, Name: view.Name, SoftwareVersion: view.Version,
		Attached: view.State == "attached", PID: view.PID}
}

func (s *localShell) attach(c net.Conn) error {
	terminal, m, err := acceptAttachment(c, &s.endpoints)
	if err != nil {
		return err
	}
	p := newAttachment(c, terminal)
	if err := s.attachManaged(p, m, s.start, false); err != nil {
		if errors.Is(err, errShellExited) || errors.Is(err, errShellAttached) {
			err = fmt.Errorf("local shell unavailable or already attached")
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

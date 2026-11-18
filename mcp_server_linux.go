//go:build linux

package main

import (
	"context"
	"fmt"
	"github.com/jay-waves/sshore/internal/mcp"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// MCP Bash mechanisms live in SessionsManager; their account identity and
// lifetime registration belong to the same table as terminal and resource tasks.
type mcpSessionTask struct {
	owner     *trackedMCPSessions
	id, start string
	pid       int
	mu        sync.Mutex
	closed    bool
	closing   bool
}

func (t *mcpSessionTask) taskName() string       { return "mcp-" + t.id }
func (t *mcpSessionTask) expired(time.Time) bool { t.mu.Lock(); defer t.mu.Unlock(); return t.closed }
func (t *mcpSessionTask) serveTask(c net.Conn, m message) error {
	switch m.Type {
	case "status":
		return writeMessage(c, message{Type: "session", Name: t.taskName(), Instance: t.id, PID: t.pid, SoftwareVersion: version})
	case "kill":
		_, err := t.owner.Close(context.Background(), mcp.SessionInput{SessionID: t.id})
		reply := message{Type: "killed"}
		if err != nil {
			reply.Type, reply.Error = "error", err.Error()
		}
		return writeMessage(c, reply)
	default:
		return fmt.Errorf("unsupported MCP task operation")
	}
}

type trackedMCPSessions struct {
	manager *mcp.SessionsManager
	tasks   *daemonTasks
	check   func() error
}

func newTrackedMCPSessions(tasks *daemonTasks, check ...func() error) *trackedMCPSessions {
	s := &trackedMCPSessions{manager: mcp.NewSessionsManager(), tasks: tasks}
	if len(check) > 0 {
		s.check = check[0]
	}
	return s
}
func (s *trackedMCPSessions) Open(ctx context.Context, in mcp.OpenInput) (mcp.Session, error) {
	gate, err := setupLock()
	if err != nil {
		return mcp.Session{}, err
	}
	defer gate.Close()
	if s.check != nil {
		if err := s.check(); err != nil {
			return mcp.Session{}, err
		}
	}
	session, err := s.manager.Open(ctx, in)
	if err != nil {
		return session, err
	}
	pid, err := s.manager.PID(session.ID)
	if err != nil {
		_, _ = s.manager.Close(ctx, mcp.SessionInput{SessionID: session.ID})
		return mcp.Session{}, err
	}
	start, err := processStart(pid)
	if err == nil {
		err = s.tasks.register(session.ID, &mcpSessionTask{owner: s, id: session.ID, pid: pid, start: start})
	}
	if err != nil {
		_, _ = s.manager.Close(ctx, mcp.SessionInput{SessionID: session.ID})
		return mcp.Session{}, err
	}
	return session, nil
}
func (s *trackedMCPSessions) task(id string) (*mcpSessionTask, error) {
	s.tasks.mu.Lock()
	task, ok := s.tasks.records[id].(*mcpSessionTask)
	s.tasks.mu.Unlock()
	if !ok || task.owner != s {
		return nil, fmt.Errorf("session not found")
	}
	task.mu.Lock()
	unavailable := task.closed || task.closing
	task.mu.Unlock()
	if unavailable {
		return nil, fmt.Errorf("session is closing or closed")
	}
	return task, nil
}
func (s *trackedMCPSessions) List(ctx context.Context, in mcp.OpenInput) (mcp.Sessions, error) {
	result, err := s.manager.List(ctx, in)
	if err != nil {
		return result, err
	}
	visible := result.Sessions[:0]
	for _, session := range result.Sessions {
		if _, err := s.task(session.ID); err == nil {
			visible = append(visible, session)
		}
	}
	result.Sessions = visible
	return result, nil
}
func (s *trackedMCPSessions) Execute(ctx context.Context, in mcp.ExecuteInput) (mcp.Execution, error) {
	if _, err := s.task(in.SessionID); err != nil {
		return mcp.Execution{}, err
	}
	return s.manager.Execute(ctx, in)
}
func (s *trackedMCPSessions) Read(ctx context.Context, in mcp.ReadInput) (mcp.Output, error) {
	if _, err := s.task(in.SessionID); err != nil {
		return mcp.Output{}, err
	}
	return s.manager.Read(ctx, in)
}
func (s *trackedMCPSessions) Close(ctx context.Context, in mcp.SessionInput) (mcp.Closed, error) {
	s.tasks.mu.Lock()
	task, ok := s.tasks.records[in.SessionID].(*mcpSessionTask)
	s.tasks.mu.Unlock()
	if !ok || task.owner != s {
		return mcp.Closed{}, fmt.Errorf("session not found")
	}
	task.mu.Lock()
	if task.closed || task.closing {
		task.mu.Unlock()
		return mcp.Closed{}, fmt.Errorf("session is closing or closed")
	}
	task.closing = true
	task.mu.Unlock()
	defer func() { task.mu.Lock(); task.closing = false; task.mu.Unlock() }()
	if err := stopOwnedProcesses([]setupProcess{{PID: task.pid, Start: task.start}}); err != nil {
		return mcp.Closed{}, err
	}
	result, err := s.manager.Close(ctx, in)
	if err == nil {
		task.mu.Lock()
		task.closed = true
		task.mu.Unlock()
	}
	return result, err
}
func (s *trackedMCPSessions) closeAll() {
	s.tasks.mu.Lock()
	var ids []string
	for id, task := range s.tasks.records {
		if task, ok := task.(*mcpSessionTask); ok && task.owner == s {
			ids = append(ids, id)
		}
	}
	s.tasks.mu.Unlock()
	for _, id := range ids {
		_, _ = s.Close(context.Background(), mcp.SessionInput{SessionID: id})
	}
}

// Recheck under the account gate: setup may have changed since SSH negotiated.
func checkMCPSessionSetup() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable = strings.TrimSuffix(executable, " (deleted)")
	state, err := inspectSetup(executable)
	if err != nil {
		return err
	}
	if state.Installed != version || state.Manual || state.Daemon != nil {
		return fmt.Errorf("remote setup changed; reconnect and review before opening a session")
	}
	return nil
}

// bridgeMCP keeps the SSH stream open while each request uses a daemon connection.
func bridgeMCP(target string) error {
	for {
		req, err := mcp.ReadRequest(os.Stdin)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if req.Version != version {
			return fmt.Errorf("release version mismatch")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		record, err := ensureLocalDaemonContext(ctx, target)
		cancel()
		if err != nil {
			return err
		}
		var reply mcp.Response
		if err := daemonJSON(context.Background(), record, "/mcp", req, &reply); err != nil {
			return err
		}
		if err := mcp.WriteResponse(os.Stdout, reply); err != nil {
			return err
		}
	}
}

func (t *mcpSessionTask) stopTask() error {
	_, err := t.owner.Close(context.Background(), mcp.SessionInput{SessionID: t.id})
	return err
}

//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jay-waves/sshore/internal/mcp"
	"github.com/jay-waves/sshore/internal/proc"
)

// The account task table owns MCP identities and lifetimes.
type mcpSessionTask struct {
	session *mcp.BashSession
	id      string
	process processIdentity
	state   atomic.Uint32
}

const (
	mcpSessionOpen = iota
	mcpSessionClosing
	mcpSessionClosed
)

func (t *mcpSessionTask) taskName() string       { return "mcp-" + t.id }
func (t *mcpSessionTask) expired(time.Time) bool { return t.state.Load() == mcpSessionClosed }
func (t *mcpSessionTask) serveTask(c net.Conn, m message) error {
	switch m.Type {
	case "status":
		return writeMessage(c, message{Type: "session", Name: t.taskName(), Instance: t.id, PID: t.process.PID, SoftwareVersion: version})
	case "kill":
		reply := message{Type: "killed"}
		if err := t.stopTask(); err != nil {
			reply.Type, reply.Error = "error", err.Error()
		}
		return writeMessage(c, reply)
	default:
		return fmt.Errorf("unsupported MCP task operation")
	}
}

type trackedMCPSessions struct {
	tasks *daemonTasks
	check func() error
}

func newTrackedMCPSessions(tasks *daemonTasks, check ...func() error) *trackedMCPSessions {
	s := &trackedMCPSessions{tasks: tasks}
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
	if len(liveTasks[*mcpSessionTask](s.tasks)) >= 16 {
		return mcp.Session{}, fmt.Errorf("session limit reached")
	}
	id := newTaskUID()
	session, err := mcp.StartBashSession(ctx)
	if err != nil {
		return mcp.Session{}, err
	}
	pid := session.PID()
	start, err := proc.StartToken(pid)
	if err == nil {
		err = s.tasks.register(id, &mcpSessionTask{session: session, id: id, process: processIdentity{PID: pid, Start: start}})
	}
	if err != nil {
		_ = session.Close()
		return mcp.Session{}, err
	}
	return mcp.Session{ID: id, Host: in.Host, State: "ready"}, nil
}
func (s *trackedMCPSessions) task(id string) (*mcpSessionTask, error) {
	task, ok := s.tasks.lookup(id).(*mcpSessionTask)
	if !ok {
		return nil, fmt.Errorf("session not found")
	}
	if !task.available() {
		return nil, fmt.Errorf("session is closing or closed")
	}
	return task, nil
}
func (t *mcpSessionTask) available() bool {
	return t.state.Load() == mcpSessionOpen
}
func (s *trackedMCPSessions) List(_ context.Context, in mcp.OpenInput) (mcp.Sessions, error) {
	tasks := liveTasks[*mcpSessionTask](s.tasks)
	result := mcp.Sessions{Sessions: make([]mcp.Session, 0, len(tasks))}
	for _, task := range tasks {
		if task.available() {
			result.Sessions = append(result.Sessions, mcp.Session{ID: task.id, Host: in.Host, State: task.session.State()})
		}
	}
	return result, nil
}
func (s *trackedMCPSessions) Execute(ctx context.Context, in mcp.ExecuteInput) (mcp.Execution, error) {
	task, err := s.task(in.SessionID)
	if err != nil {
		return mcp.Execution{}, err
	}
	return task.session.Execute(ctx, in)
}
func (s *trackedMCPSessions) Read(ctx context.Context, in mcp.ReadInput) (mcp.Output, error) {
	task, err := s.task(in.SessionID)
	if err != nil {
		return mcp.Output{}, err
	}
	return task.session.Read(ctx, in)
}
func (s *trackedMCPSessions) Close(_ context.Context, in mcp.SessionInput) (mcp.Closed, error) {
	task, err := s.task(in.SessionID)
	if err != nil {
		return mcp.Closed{}, err
	}
	if err := task.stopTask(); err != nil {
		return mcp.Closed{}, err
	}
	return mcp.Closed{Closed: true}, nil
}

func (t *mcpSessionTask) stopTask() error {
	if !t.state.CompareAndSwap(mcpSessionOpen, mcpSessionClosing) {
		return fmt.Errorf("session is closing or closed")
	}
	// Failed cleanup leaves the same registered task available for retry.
	defer t.state.CompareAndSwap(mcpSessionClosing, mcpSessionOpen)
	if err := stopOwnedProcesses([]setupProcess{{PID: t.process.PID, Start: t.process.Start}}); err != nil {
		return err
	}
	if err := t.session.Close(); err != nil {
		return err
	}
	t.state.Store(mcpSessionClosed)
	return nil
}
func (s *trackedMCPSessions) closeAll() {
	for _, task := range liveTasks[*mcpSessionTask](s.tasks) {
		_ = task.stopTask()
	}
}

// Recheck under the account gate: setup may have changed since SSH negotiated.
func checkMCPSessionSetup() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable = strings.TrimSuffix(executable, " (deleted)")
	return requireInstalledRelease(executable)
}

// An SSH stream is bound to the daemon selected during setup. A daemon restart
// ends that stream on its next request; the client can establish a fresh one.
func bridgeMCP(record localShellRecord) error {
	return mcp.Relay(os.Stdin, os.Stdout, func(req json.RawMessage) (json.RawMessage, error) {
		var reply json.RawMessage
		err := daemonJSONLimit(context.Background(), record, "/mcp", req, &reply, mcp.MaxRPCPayload+1)
		return reply, err
	})
}

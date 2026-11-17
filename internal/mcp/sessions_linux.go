//go:build linux

package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jay-waves/sshore/internal/tmux"
)

type managedSession struct {
	shell      *tmux.Bash
	executions []sessionExecution
}
type sessionExecution struct {
	id        string
	execution *tmux.BashExecution
}
type SessionsManager struct {
	mu       sync.Mutex
	sessions map[string]*managedSession
}

func NewSessionsManager() *SessionsManager {
	return &SessionsManager{sessions: make(map[string]*managedSession)}
}
func (m *SessionsManager) Open(ctx context.Context, in OpenInput) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) >= 16 {
		return Session{}, fmt.Errorf("session limit reached")
	}
	b, err := tmux.StartBash()
	if err != nil {
		return Session{}, err
	}
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	m.sessions[id] = &managedSession{shell: b}
	return Session{ID: id, Host: in.Host, State: "ready"}, nil
}
func (m *SessionsManager) List(_ context.Context, in OpenInput) (Sessions, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := Sessions{Sessions: make([]Session, 0, len(m.sessions))}
	for id, session := range m.sessions {
		state := "ready"
		if !session.shell.Alive() {
			state = "exited"
		} else if !session.shell.Usable() {
			state = "unavailable"
		}
		result.Sessions = append(result.Sessions, Session{ID: id, Host: in.Host, State: state})
	}
	return result, nil
}
func (m *SessionsManager) Execute(ctx context.Context, in ExecuteInput) (Execution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[in.SessionID]
	if s == nil {
		return Execution{}, fmt.Errorf("session not found")
	}
	if err := ctx.Err(); err != nil {
		return Execution{}, err
	}
	if len(s.executions) >= 16 {
		oldest := -1
		for i, record := range s.executions {
			execution := record.execution
			select {
			case <-execution.Done:
				select {
				case <-execution.OutputDone:
					oldest = i
				default:
				}
			default:
			}
			if oldest >= 0 {
				break
			}
		}
		if oldest < 0 {
			return Execution{}, fmt.Errorf("execution limit reached; output streams are still open")
		}
		s.executions = slices.Delete(s.executions, oldest, oldest+1)
	}
	execution, err := s.shell.Run(in.Code, in.Stdin)
	if err != nil {
		return Execution{}, err
	}
	id := rand.Text()
	s.executions = append(s.executions, sessionExecution{id, execution})
	return Execution{ID: id, State: "running"}, nil
}

func (m *SessionsManager) Read(ctx context.Context, in ReadInput) (Output, error) {
	m.mu.Lock()
	s := m.sessions[in.SessionID]
	var e *tmux.BashExecution
	if s != nil {
		for _, record := range s.executions {
			if record.id == in.ExecutionID {
				e = record.execution
				break
			}
		}
	}
	m.mu.Unlock()
	if e == nil {
		return Output{}, fmt.Errorf("execution not found")
	}
	outPos, errPos, err := parseCursor(in.Cursor)
	if err != nil {
		return Output{}, err
	}
	result, resultErr, changed := e.Snapshot()
	if outPos > len(result.Stdout) || errPos > len(result.Stderr) {
		return Output{}, fmt.Errorf("invalid output cursor")
	}
	if outPos == len(result.Stdout) && errPos == len(result.Stderr) && !result.OutputClosed && resultErr == nil && in.WaitMS > 0 {
		timer := time.NewTimer(time.Duration(in.WaitMS) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Output{}, ctx.Err()
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
		result, resultErr, _ = e.Snapshot()
	}
	state := "running"
	errorText := ""
	if resultErr != nil {
		state = "unknown"
		errorText = resultErr.Error()
	}
	var code *int
	if result.Complete {
		state = "completed"
		n := result.ExitCode
		code = &n
	}
	outEnd := min(len(result.Stdout), outPos+4096)
	errEnd := min(len(result.Stderr), errPos+4096)
	stdout, stdout64 := outputChunk(result.Stdout[outPos:outEnd])
	stderr, stderr64 := outputChunk(result.Stderr[errPos:errEnd])
	return Output{Stdout: stdout, StdoutBase64: stdout64, Stderr: stderr, StderrBase64: stderr64, Cursor: fmt.Sprintf("%d:%d", outEnd, errEnd), State: state, Error: errorText, ExitCode: code, OutputClosed: result.OutputClosed && outEnd == len(result.Stdout) && errEnd == len(result.Stderr), Truncated: result.Truncated}, nil
}
func parseCursor(cursor string) (int, int, error) {
	if cursor == "" {
		return 0, 0, nil
	}
	a, b, ok := strings.Cut(cursor, ":")
	x, e1 := strconv.Atoi(a)
	y, e2 := strconv.Atoi(b)
	if !ok || e1 != nil || e2 != nil || x < 0 || y < 0 {
		return 0, 0, fmt.Errorf("invalid output cursor")
	}
	return x, y, nil
}
func (m *SessionsManager) Close(_ context.Context, in SessionInput) (Closed, error) {
	m.mu.Lock()
	s := m.sessions[in.SessionID]
	if s != nil {
		delete(m.sessions, in.SessionID)
	}
	m.mu.Unlock()
	if s == nil {
		return Closed{}, fmt.Errorf("session not found")
	}
	if err := s.shell.Close(); err != nil {
		return Closed{}, err
	}
	return Closed{Closed: true}, nil
}

func outputChunk(data []byte) (string, string) {
	if utf8.Valid(data) {
		return string(data), ""
	}
	return "", base64.StdEncoding.EncodeToString(data)
}

// PID returns the owned Bash process for lifecycle management.
func (m *SessionsManager) PID(id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return 0, fmt.Errorf("session not found")
	}
	return s.shell.PID(), nil
}

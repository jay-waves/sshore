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

// mcpRegistryEntry makes a persistent Bash visible to the existing account
// daemon scan while the shared daemon owns the session.
type mcpRegistryEntry struct {
	path     string
	lock     *os.File
	listener net.Listener
	done     chan struct{}
	once     sync.Once
	pid      int
	start    string
}

func registerMCPSession(id string) (*mcpRegistryEntry, error) {
	name := "mcp-" + id
	if err := validSession(name); err != nil {
		return nil, err
	}
	path, err := socketPath(name)
	if err != nil {
		return nil, err
	}
	lock, err := openLock(path+".lock", true)
	if err != nil {
		return nil, fmt.Errorf("session identity already in use: %w", err)
	}
	fail := func(e error) (*mcpRegistryEntry, error) { lock.Close(); return nil, e }
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fail(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return fail(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		os.Remove(path)
		return fail(err)
	}
	entry := &mcpRegistryEntry{path: path, lock: lock, listener: listener, done: make(chan struct{})}
	go func() {
		defer close(entry.done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				req, err := readMessage(conn)
				if err != nil {
					return
				}
				if req.Type == "status" && req.Action == "health" {
					_ = writeMessage(conn, message{Type: "session", Name: name, PID: os.Getpid(), SoftwareVersion: version})
				}
			}()
		}
	}()
	return entry, nil
}
func (e *mcpRegistryEntry) Close() {
	e.once.Do(func() {
		_ = e.listener.Close()
		<-e.done
		_ = os.Remove(e.path)
		_ = e.lock.Close()
	})
}

// trackedMCPSessions registers Bash processes under the same setup lock used by
// remote shell creation and replacement.
type trackedMCPSessions struct {
	manager *mcp.SessionsManager
	mu      sync.Mutex
	entries map[string]*mcpRegistryEntry
	check   func() error
}

func newTrackedMCPSessions(check ...func() error) *trackedMCPSessions {
	s := &trackedMCPSessions{manager: mcp.NewSessionsManager(), entries: make(map[string]*mcpRegistryEntry)}
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
	if err != nil {
		_, _ = s.manager.Close(ctx, mcp.SessionInput{SessionID: session.ID})
		return mcp.Session{}, err
	}
	entry, err := registerMCPSession(session.ID)
	if err != nil {
		_, _ = s.manager.Close(ctx, mcp.SessionInput{SessionID: session.ID})
		return mcp.Session{}, err
	}
	entry.pid, entry.start = pid, start
	s.mu.Lock()
	s.entries[session.ID] = entry
	s.mu.Unlock()
	return session, nil
}
func (s *trackedMCPSessions) List(ctx context.Context, in mcp.OpenInput) (mcp.Sessions, error) {
	return s.manager.List(ctx, in)
}
func (s *trackedMCPSessions) Execute(ctx context.Context, in mcp.ExecuteInput) (mcp.Execution, error) {
	return s.manager.Execute(ctx, in)
}
func (s *trackedMCPSessions) Read(ctx context.Context, in mcp.ReadInput) (mcp.Output, error) {
	return s.manager.Read(ctx, in)
}
func (s *trackedMCPSessions) Close(ctx context.Context, in mcp.SessionInput) (mcp.Closed, error) {
	s.mu.Lock()
	entry := s.entries[in.SessionID]
	if entry == nil {
		s.mu.Unlock()
		return mcp.Closed{}, fmt.Errorf("session not found")
	}
	// Prevent concurrent close from signalling a reused PID or removing a new entry.
	delete(s.entries, in.SessionID)
	s.mu.Unlock()
	if err := stopOldSessions([]setupProcess{{PID: entry.pid, Start: entry.start}}, "cancelled by sshore"); err != nil {
		s.mu.Lock()
		s.entries[in.SessionID] = entry
		s.mu.Unlock()
		return mcp.Closed{}, err
	}
	result, err := s.manager.Close(ctx, in)
	if err != nil {
		s.mu.Lock()
		s.entries[in.SessionID] = entry
		s.mu.Unlock()
		return result, err
	}
	entry.Close()
	return result, nil
}

func (s *trackedMCPSessions) closeAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	s.mu.Unlock()
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
	if state.Installed != version || state.Manual || len(state.Processes) != 0 {
		return fmt.Errorf("remote setup changed; reconnect and review before opening a session")
	}
	return nil
}

// bridgeMCP keeps the SSH stream open while each request uses a daemon connection.
func bridgeMCP(c net.Conn, target string) error {
	first := true
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
		if !first {
			c, err = connectRemoteDaemon(target)
			if err != nil {
				return err
			}
		}
		first = false
		err = func() error {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(40 * time.Second))
			if err := writeMessage(c, message{Type: "mcp_request", SoftwareVersion: version, MCPRequest: &req}); err != nil {
				return err
			}
			reply, err := readMessage(c)
			if err != nil {
				return err
			}
			if reply.Type != "mcp_response" || reply.SoftwareVersion != version || reply.MCPResponse == nil {
				return fmt.Errorf("invalid daemon response")
			}
			return mcp.WriteResponse(os.Stdout, *reply.MCPResponse)
		}()
		if err != nil {
			return err
		}
	}
}

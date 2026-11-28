package tmux

import (
	"errors"
	"slices"
	"sync"
	"time"
)

// Manager owns a daemon's live terminal sessions and their parent tree.
// Its zero value is ready to use.
type Manager struct {
	mu       sync.Mutex
	forest   forest
	sessions map[string]*Session
}

type SessionConfig struct {
	ID, Name     string
	Parent       *Session
	Process      ProcessConfig
	StartTimeout time.Duration
	RetryStart   bool
	Events       Events
}

// Create registers a session. The first attachment starts its process.
func (m *Manager) Create(config SessionConfig) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if config.ID == "" || m.sessions[config.ID] != nil {
		return nil, errors.New("invalid or duplicate session ID")
	}
	if config.Parent != nil && config.Parent.manager != m {
		return nil, errors.New("parent belongs to another session manager")
	}
	done := make(chan struct{})
	config.Process.Environment = slices.Clone(config.Process.Environment)
	session := &Session{events: config.Events, manager: m, name: config.Name,
		config: config.Process, retryStart: config.RetryStart, node: &treeNode{id: config.ID, Label: config.ID[:min(6, len(config.ID))], done: done}, done: done}
	if m.sessions == nil {
		m.sessions = make(map[string]*Session)
	}
	m.sessions[config.ID] = session
	if parent := config.Parent; parent != nil {
		m.forest.setParent(session.node, parent.node)
	}
	if config.StartTimeout > 0 {
		session.expireUnstarted(config.StartTimeout)
	}
	if parent := config.Parent; parent != nil {
		session.watchParent(parent.node)
	}
	return session, nil
}

func (m *Manager) Lookup(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *Manager) Children(parent *Session) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var children []*Session
	for _, session := range m.sessions {
		if session.node.Parent() == parent.node {
			children = append(children, session)
		}
	}
	return children
}

func (m *Manager) remove(t *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, t.node.id)
}

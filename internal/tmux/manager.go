package tmux

import (
	"errors"
	"sync"
)

// Manager owns the live sessions for one account daemon. A published task ID
// can resolve only to a session registered here by this daemon.
type Manager[A attachmentOwner, T any] struct {
	mu       sync.Mutex
	forest   Forest[T]
	sessions map[string]*Task[A, T]
}

// NewSession prepares a session for registration. The caller supplies the ID
// and name reserved in the account's shared shell and resource namespace.
func (m *Manager[A, T]) NewSession(id, name string, value T, parent *Task[A, T], guard *sync.Mutex, events Events[A, T]) *Task[A, T] {
	done := make(chan struct{})
	t := &Task[A, T]{guard: guard, events: events, manager: m, name: name,
		node: &treeNode[T]{id: id, Value: value, Label: id[:min(6, len(id))], done: done}, done: done}
	if parent != nil {
		m.forest.setParent(t.node, parent.node)
	}
	return t
}

// Register makes a prepared session visible and starts tracking its parent.
func (m *Manager[A, T]) Register(t *Task[A, T]) error {
	m.mu.Lock()
	if m.sessions[t.node.id] != nil {
		m.mu.Unlock()
		return errors.New("session already exists")
	}
	if m.sessions == nil {
		m.sessions = make(map[string]*Task[A, T])
	}
	parent := t.node.Parent()
	m.sessions[t.node.id] = t
	m.mu.Unlock()
	if parent != nil {
		t.watchParent(parent)
	}
	return nil
}

func (m *Manager[A, T]) Lookup(id string) (value T, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.sessions[id]; t != nil {
		return t.node.Value, true
	}
	return value, false
}

func (m *Manager[A, T]) Sessions() []T {
	m.mu.Lock()
	defer m.mu.Unlock()
	values := make([]T, 0, len(m.sessions))
	for _, t := range m.sessions {
		values = append(values, t.node.Value)
	}
	return values
}

func (m *Manager[A, T]) Children(parent *Task[A, T]) []T {
	m.mu.Lock()
	defer m.mu.Unlock()
	var values []T
	for _, t := range m.sessions {
		if t.node.Parent() == parent.node {
			values = append(values, t.node.Value)
		}
	}
	return values
}

// Remove the session and publish completion under the same lock, before any
// later full-ID lookup can return the finished session.
func (m *Manager[A, T]) complete(t *Task[A, T]) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[t.node.id] == t {
		delete(m.sessions, t.node.id)
	}
	close(t.done)
}

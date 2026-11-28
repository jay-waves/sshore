package main

import (
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/tmux"
)

var errTerminalEndpoint = errors.New("terminal endpoint expired or already claimed")

type pendingTerminal struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (t *pendingTerminal) Close() error {
	var err error
	t.once.Do(func() {
		err = t.Conn.Close()
		close(t.done)
	})
	return err
}

// Pair the SSH or loopback data channel with one authenticated attach request.
func (s *shellTask) offerTerminal(conn net.Conn) error {
	state := s.session.Snapshot()
	if state.State == tmux.StateExited || state.Stopping {
		return tmux.ErrEnded
	}
	terminal := &pendingTerminal{Conn: conn, done: make(chan struct{})}
	defer terminal.Close()
	token := rand.Text()
	s.mu.Lock()
	if len(s.terminals) >= 8 {
		s.mu.Unlock()
		return errors.New("too many pending terminal endpoints")
	}
	if s.terminals == nil {
		s.terminals = make(map[string]*pendingTerminal)
	}
	s.terminals[token] = terminal
	s.mu.Unlock()
	defer s.takeTerminal(token)

	timer := time.AfterFunc(5*time.Second, func() {
		if unused := s.takeTerminal(token); unused != nil {
			_ = unused.Close()
		}
	})
	defer timer.Stop()
	if err := streamMessage(conn, message{Type: "terminal_ready", SoftwareVersion: version, Token: token}); err != nil {
		return err
	}
	select {
	case <-terminal.done:
	case <-s.session.Done():
	}
	return nil
}

func (s *shellTask) takeTerminal(token string) *pendingTerminal {
	s.mu.Lock()
	defer s.mu.Unlock()
	terminal := s.terminals[token]
	delete(s.terminals, token)
	return terminal
}

package main

import (
	"os"
	"strings"
	"sync"

	shellmux "github.com/jay-waves/sshore/internal/mux"
)

type localShell struct {
	processIdentity string
	environment     []string
	nameLock        *os.File
	mu              sync.Mutex
	record          localSessionRecord
	terminal        *shellmux.Terminal
	lifecycle       shellmux.Lifecycle[*attachment]
	modes           shellmux.InputState
	done            chan struct{}
	once            sync.Once
}

func (s *localShell) close() {
	s.once.Do(func() {
		s.mu.Lock()
		terminal, owner := s.terminal, s.lifecycle.End()
		temporary, identity := s.record.Owner.PID != 0, s.processIdentity
		s.mu.Unlock()
		if owner != nil {
			owner.mux.Close()
		}
		if terminal != nil {
			if temporary {
				_ = stopSpawnTerminal(terminal.PID(), identity)
			}
			terminal.Close()
		}
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
	path, args := localShellCommand(source)
	env := make([]string, 0, len(source)+2)
	for _, item := range source {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "SSHORE_SOCKET", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "SSHORE_LOCAL_SESSION", "TERM":
			continue
		}
		env = append(env, item)
	}
	env = append(env, "TERM="+m.Term, "SSHORE_LOCAL_SESSION="+s.record.identity())
	t, err := shellmux.Start(shellmux.Config{Path: path, Args: args, Env: env, Dir: s.record.Directory, Size: shellmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}})
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
						p.mux.Close()
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
			p.finishExit(code)
		}
		s.close()
	}()
	return nil
}

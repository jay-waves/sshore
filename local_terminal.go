package main

import (
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/jay-waves/sshore/internal/tmux"
)

type localShell struct {
	processIdentity string
	environment     []string
	nameLock        *os.File
	mu              sync.Mutex
	record          localSessionRecord
	terminal        *tmux.Terminal
	lifecycle       tmux.Lifecycle[*attachment]
	modes           tmux.InputState
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
			owner.Mux().Close()
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
	env := localShellEnvironment(source, m.Term, s.record.identity())
	t, err := tmux.Start(tmux.Config{Path: path, Args: args, Env: env, Dir: s.record.Directory, Size: tmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}})
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
						p.Mux().Close()
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
			p.FinishExit(code)
		}
		s.close()
	}()
	return nil
}

func localShellEnvironment(source []string, term, identity string) []string {
	env := make([]string, 0, len(source)+2)
	for _, item := range source {
		key, _, _ := strings.Cut(item, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		switch key {
		case "SSHORE_SOCKET", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "SSHORE_LOCAL_SESSION", "TERM":
			continue
		}
		env = append(env, item)
	}
	env = append(env, "TERM="+term, "SSHORE_LOCAL_SESSION="+identity)
	return env
}

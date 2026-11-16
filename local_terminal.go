package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/jay-waves/sshore/internal/shell"
	statefs "github.com/jay-waves/sshore/internal/state"
	"github.com/jay-waves/sshore/internal/tmux"
)

type localShell struct {
	processIdentity string
	environment     []string
	nameLock        *os.File
	mu              sync.Mutex
	record          localShellRecord
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
		saveShellTree(s.record.identity(), s.record.Name, nil, "exited")
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
	path := localShellPath(source)
	args, err := shell.StartupArgs(path, m.NoLogin, m.NoProfile)
	if err != nil {
		return err
	}
	env := shell.StartupEnv(path, localShellEnvironment(source, m.Term, s.record.identity()), m.NoProfile)
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
		case "SSHORE_SOCKET", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "SSHORE_LOCAL_SESSION", "SSHORE_TREE_ID", "TERM":
			continue
		}
		env = append(env, item)
	}
	env = append(env, "TERM="+term, "SSHORE_LOCAL_SESSION="+identity, "SSHORE_TREE_ID="+identity)
	return env
}

func (s *localShell) rename(name string) error {
	if err := validSession(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle.Exited() {
		return fmt.Errorf("local shell has ended")
	}
	if s.record.Name == name {
		return nil
	}
	reservation, err := reserveResourceName("shell", name)
	if err != nil {
		return err
	}
	defer reservation.Close()
	next := s.record
	next.Name = name
	path, err := localSessionPath(name)
	if err != nil {
		return err
	}
	if err = statefs.WriteJSON(path, next); err != nil {
		return err
	}
	oldPath, _ := localSessionPath(s.record.Name)
	if err = os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
		os.Remove(path)
		return err
	}
	s.record = next
	renameShellTree(s.record.identity(), name)
	s.nameLock.Close()
	s.nameLock = reservation.owner
	reservation.owner = nil
	return nil
}

func (s *localShell) targets(request message) message {
	s.mu.Lock()
	name := s.record.Name
	s.mu.Unlock()
	reply := message{Type: "session_targets", Action: request.Action, Name: name}
	if request.Action == "new" {
		reservation, err := reserveResourceName("shell", "")
		if err != nil {
			reply.Error = err.Error()
			return reply
		}
		reply.Names = []string{reservation.name}
		reservation.Close()
		return reply
	}
	rows, err := localShells()
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
		reply.Names = append(reply.Names, row.Name)
	}
	if request.Action == "select" && len(reply.Names) != 1 {
		reply.Error = fmt.Sprintf("local shell %s unavailable", cleanText(request.Name))
	}
	if len(reply.Names) > 128 {
		reply.Names = nil
		reply.Error = "too many local shells; use sshore list --local"
	}
	return reply
}

func detachLocalShell(identity string) error {
	rows, err := localShells()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.identity != identity {
			continue
		}
		m, err := localRequest(row.Name, message{Type: "detach"}, identity)
		if err != nil {
			return err
		}
		if m.Type != "detach_requested" && m.Type != "detached" {
			return fmt.Errorf("local detach unconfirmed")
		}
		return nil
	}
	return fmt.Errorf("local shell is unavailable")
}

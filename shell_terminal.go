package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
	"github.com/jay-waves/sshore/internal/pty"
)

// A daemon-owned shell with one terminal and at most one attachment. Entry
// points supply owner supervision and their additional commands.
type shellTask struct {
	endpoints       terminalEndpoints
	mu              sync.Mutex // Protect shell state and attachment ownership.
	terminalMu      sync.Mutex // Serialize replay and redraw with live output. Acquire before mu.
	renameMu        sync.Mutex // Keep each shell's history renames in live update order.
	terminal        *pty.Terminal
	process         processIdentity
	modes           pty.InputState
	active          *attachment
	exited          bool
	tasks           *daemonTasks
	record          localShellRecord
	environment     []string
	done            chan struct{}
	once            sync.Once
	hosted, killing bool
	started         time.Time
	exitReason      string
}

func (s *shellTask) taskName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record.Name
}

func (s *shellTask) expired(time.Time) bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *shellTask) shellEnvironment() []string {
	if s.environment != nil {
		return s.environment
	}
	return os.Environ()
}

func (s *shellTask) treeIdentity() string {
	if s.hosted {
		return remoteTreeIdentity(s.record.UID)
	}
	return s.record.identity()
}

// Call with mu held.
func (s *shellTask) finishLocked() {
	s.once.Do(func() {
		saveShellTree(s.treeIdentity(), s.record.Name, nil, "exited")
		close(s.done)
	})
}

func (s *shellTask) close() {
	s.once.Do(func() {
		s.mu.Lock()
		terminal, attachment := s.terminal, s.endTerminal()
		stop := s.record.Owner.PID != 0
		process, identity, name := s.process, s.treeIdentity(), s.record.Name
		s.mu.Unlock()
		if attachment != nil {
			attachment.Close()
		}
		if terminal != nil {
			if stop {
				_ = stopSpawnTerminal(process.PID, process.Start)
			}
			_ = terminal.Close()
		}
		saveShellTree(identity, name, nil, "exited")
		close(s.done)
	})
}

func (s *shellTask) stopTask() error {
	return s.kill(s.record.UID, "killed by sshore kill")
}

func (s *shellTask) kill(instance, reason string) error {
	s.mu.Lock()
	if s.hosted && (instance == "" || instance != s.record.UID) {
		s.mu.Unlock()
		return fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	process := s.process
	s.killing = true
	if s.exitReason == "" {
		s.exitReason = reason
	}
	if s.hosted && process.PID == 0 {
		s.endTerminal()
		s.finishLocked()
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if err := s.signal(process); err != nil {
		s.mu.Lock()
		s.killing = false
		s.mu.Unlock()
		return err
	}
	if !s.hosted {
		s.close()
	} else {
		s.mu.Lock()
		s.finishLocked()
		s.mu.Unlock()
	}
	return nil
}

func (s *shellTask) signal(process processIdentity) error {
	if process.PID == 0 {
		return nil
	}
	return stopSpawnTerminal(process.PID, process.Start)
}

func (s *shellTask) startManagedShell(path string, env []string, m message) error {
	err := s.startShell(path, env, s.record.Directory, m, func(exitErr error, code int) {
		s.mu.Lock()
		p := s.endTerminal()
		reason := s.exitReason
		if reason == "" {
			reason = "shell exited"
			if s.hosted {
				reason = fmt.Sprintf("shell exited (code %d)", code)
				if e, ok := errors.AsType[*exec.ExitError](exitErr); ok {
					if status, ok := e.Sys().(syscall.WaitStatus); ok && status.Signaled() {
						reason = "shell terminated by " + status.Signal().String()
					}
				}
			} else if exitErr != nil {
				reason = exitErr.Error()
			}
			s.exitReason = reason
		}
		s.mu.Unlock()
		if p != nil {
			p.FinishExit(code, reason)
		}
		if !s.hosted {
			s.close()
		} else {
			s.mu.Lock()
			s.finishLocked()
			s.mu.Unlock()
		}
	})
	if err == nil {
		s.started = time.Now()
	}
	return err
}

// Call with mu held.
func (s *shellTask) state() string {
	switch {
	case s.exited:
		return "unavailable"
	case s.active != nil:
		return "attached"
	case s.process.PID == 0:
		return "starting"
	default:
		return "detached"
	}
}

// The table excludes new claims while the shell verifies the old reference.
// renameMu keeps the history updates in the same order as live renames.
func (s *shellTask) rename(old, name, instance string) error {
	if err := validAlias(name); err != nil {
		return err
	}
	s.renameMu.Lock()
	defer s.renameMu.Unlock()
	s.tasks.mu.Lock()
	if err := s.tasks.checkNameLocked(s.record.UID, name, time.Now()); err != nil {
		s.tasks.mu.Unlock()
		return err
	}
	s.mu.Lock()
	changed := s.exited || s.killing || (s.hosted || old != "") && s.record.Name != old || instance != "" && instance != s.record.UID
	updated := !changed && s.record.Name != name
	identity := s.treeIdentity()
	if updated {
		s.record.Name = name
	}
	s.mu.Unlock()
	s.tasks.mu.Unlock()
	if changed {
		return fmt.Errorf("shell changed or is exiting; refresh with sshore list")
	}
	if updated {
		renameShellTree(identity, name)
	}
	return nil
}

func (s *shellTask) serveCommand(c net.Conn, m message, attach func(net.Conn) error, status func(string) message) (bool, error) {
	var reply message
	switch m.Type {
	case "terminal_open":
		return true, s.endpoints.serve(c, m)
	case "attach_control":
		if err := checkPeerVersion(m.SoftwareVersion, "client"); err != nil {
			return true, writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		}
		if err := writeMessage(c, message{Type: "attach_control_ready", SoftwareVersion: version}); err != nil {
			return true, err
		}
		return true, attach(c)
	case "status":
		reply = status(m.Action)
	case "detach":
		s.mu.Lock()
		p := s.active
		s.mu.Unlock()
		// The control loop owns final detachment.
		if p != nil {
			if err := p.controlMessage(message{Type: "detach_requested"}); err != nil {
				return true, writeMessage(c, message{Type: "error", Error: "detach request could not reach the client; reconnect to check the shell"})
			}
		}
		reply.Type = "detached"
	case "rename", "kill":
		var err error
		if s.hosted {
			err = checkPeerVersion(m.SoftwareVersion, "client")
		}
		if err == nil {
			if m.Type == "rename" {
				err = s.rename(m.Action, m.Name, m.Instance)
				reply = message{Type: "renamed", Name: m.Name}
			} else {
				err = s.kill(m.Instance, "killed by sshore kill")
				reply.Type = "killed"
			}
		}
		if err != nil {
			reply = message{Type: "error", Error: err.Error()}
		}
	default:
		return false, nil
	}
	return true, writeMessage(c, reply)
}

// Both entry points publish readiness only after a successful attach. Hosted
// shells obtain their owner context from the authenticated attachment.
func (s *shellTask) attachManaged(p *attachment, m message, start func(message) error, hostedTemporary bool) error {
	if err := validateAttach(m); err != nil {
		return err
	}
	s.terminalMu.Lock()
	s.mu.Lock()
	initial := s.terminal == nil
	if s.killing || s.hosted && (m.Instance != "" && m.Instance != s.record.UID || initial && m.Action == "existing") {
		s.mu.Unlock()
		s.terminalMu.Unlock()
		return fmt.Errorf("shell changed or is unavailable; refresh with sshore list")
	}
	err := s.attachTerminal(p, m, start, func() error {
		temporary, owner, instance := hostedTemporary, m.OwnerProcess, s.record.UID
		if !s.hosted {
			temporary, owner, instance = s.record.Owner.PID != 0, describeSpawnOwner(s.record.Owner), ""
		}
		tree := s.treeIdentity()
		saveShellTreeWithProcess(tree, s.record.Name, m.Tree, "attached", owner, shellLifetime(temporary, s.hosted), m.SourceHost, m.SourceMachineID)
		return p.controlMessage(message{Type: "ready", Tree: shellTreeFor(tree), Temporary: temporary, Instance: instance, PID: s.process.PID, Name: s.record.Name})
	})
	failedInitial := err != nil && initial && !errors.Is(err, errShellAttached) && !errors.Is(err, errShellExited)
	if failedInitial && s.hosted && s.terminal != nil {
		s.endTerminal()
		s.exitReason = "initial attachment failed"
		_ = s.terminal.Close()
		s.finishLocked()
	} else if failedInitial && !s.hosted {
		s.endTerminal()
	}
	s.mu.Unlock()
	s.terminalMu.Unlock()
	if failedInitial && !s.hosted {
		s.close()
	}
	return err
}

var (
	errShellExited   = errors.New("session is exiting")
	errShellAttached = errors.New("session already attached")
)

// Call with mu held.
func (s *shellTask) endTerminal() *attachment {
	p := s.active
	s.active, s.exited = nil, true
	return p
}

type shellInput interface {
	io.Writer
	Stopped() bool
	Stop() error
}

func shellSize(m message) pty.Size {
	return pty.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}
}

func hostedShellEnvironment(source []string, term, instance, executable string) []string {
	env := append(append([]string(nil), source...), "TERM="+term, "SSHORE_SOCKET="+instance, "SSHORE_TREE_ID="+remoteTreeIdentity(instance))
	if executable != "" {
		env = append(env, "PATH="+filepath.Dir(executable)+string(os.PathListSeparator)+localEnvironmentValue(source, "PATH"))
	}
	return env
}

// Both native and hosted sessions use the same shell startup rules; the caller
// chooses the platform-specific shell and prepares its session environment.
func (s *shellTask) startShell(path string, env []string, dir string, m message, exited func(error, int)) error {
	args, err := pty.StartupArgs(path, m.NoLogin, m.NoProfile)
	if err != nil {
		return err
	}
	return s.startTerminal(path, args, pty.StartupEnv(path, env, m.NoProfile), dir, m, exited)
}

// The caller holds mu while publishing the terminal. The exit handler owns
// task-specific state and exit reporting.
func (s *shellTask) startTerminal(path string, args, env []string, dir string, m message, exited func(error, int)) error {
	t, err := pty.Start(pty.Config{Path: path, Args: args, Env: env, Dir: dir, Size: shellSize(m)})
	if err != nil {
		return err
	}
	pid := t.PID()
	identity, err := proc.LiveIdentity(pid)
	if err != nil {
		t.Close()
		return err
	}
	s.terminal = t
	s.process = processIdentity{PID: pid, Start: identity}
	go s.pumpTerminal(t, exited)
	return nil
}

func (s *shellTask) pumpTerminal(t *pty.Terminal, exited func(error, int)) {
	buf := make([]byte, 8192)
	for {
		n, err := t.Read(buf)
		if n > 0 {
			s.terminalMu.Lock()
			s.modes.Observe(buf[:n])
			// Keep live output ordered without holding the shell lock during I/O.
			s.mu.Lock()
			p := s.active
			s.mu.Unlock()
			if p != nil && p.Output(buf[:n]) != nil {
				p.Close()
			}
			s.terminalMu.Unlock()
		}
		if err != nil {
			break
		}
	}
	err := t.Wait()
	code := 0
	if err != nil {
		code = 1
		if e, ok := errors.AsType[interface {
			error
			ExitCode() int
		}](err); ok {
			code = e.ExitCode()
		}
	}
	exited(err, code)
}

// Caller holds both host locks. The attachment acquires ownership only after
// startup, terminal setup and the ready handshake have succeeded.
func (s *shellTask) attachTerminal(p *attachment, m message, start func(message) error, ready func() error) error {
	if s.exited {
		return errShellExited
	}
	if s.active != nil {
		return errShellAttached
	}
	initial := s.terminal == nil
	if initial {
		if err := start(m); err != nil {
			return err
		}
	} else if m.NoLogin || m.NoProfile {
		return fmt.Errorf("startup options require a new managed shell")
	}
	if err := s.terminal.Resize(shellSize(m)); err != nil {
		return err
	}
	lease := pty.NewInputLease(s.terminal)
	err := ready()
	if err == nil {
		if data := s.modes.Replay(); len(data) > 0 {
			err = p.Output(data)
		}
	}
	if err != nil {
		lease.Stop()
		return err
	}
	p.input = lease
	s.active = p
	if !initial {
		pty.RequestRedraw(s.terminal)
	}
	return nil
}

// Hold mu through Stop so no successor can acquire the terminal's writer.
func (s *shellTask) detachTerminal(p *attachment) (bool, error) {
	active := s.active == p
	if p.input == nil || !active && !s.exited {
		return false, nil
	}
	input := p.input
	p.input = nil
	err := input.Stop()
	// An exited shell has no successor, but its input worker must still join.
	if !active {
		return false, nil
	}
	if err != nil {
		s.endTerminal()
		return false, err
	}
	s.active = nil
	return true, nil
}

// Once attached, only this loop reads control frames. The host records release
// under the shell lock, and may return cleanup that needs to run outside it.
func (s *shellTask) serveAttachment(p *attachment, control io.Reader, resizeAck bool, onRelease func(error) (string, func()), extra func(message) error) error {
	release := func() (string, error) {
		s.mu.Lock()
		detached, err := s.detachTerminal(p)
		var name string
		var cleanup func()
		if detached || err != nil {
			name, cleanup = onRelease(err)
		}
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return name, err
	}
	released := false
	inputDone := make(chan struct{})
	defer func() {
		p.Close()
		if !released {
			_, _ = release()
		}
		<-inputDone
	}()
	input := p.input
	go func() {
		defer close(inputDone)
		p.ReadInput(input, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.active == p
		})
	}()
	for {
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		switch m.Type {
		case "detach":
			p.approvals.disable()
			released = true
			name, err := release()
			if err == nil && name != "" {
				return p.FinishDetach(name)
			}
			return err
		case "input_barrier":
			err = p.DrainInput(m.TerminalOffset)
		case "resize":
			s.mu.Lock()
			if s.active == p {
				err = s.terminal.Resize(shellSize(m))
			}
			s.mu.Unlock()
			if err != nil {
				return fmt.Errorf("resize terminal: %w", err)
			}
			if resizeAck {
				err = p.controlMessage(message{Type: "resized", Cols: m.Cols, Rows: m.Rows})
			}
		case "console_inventory":
			err = p.controlMessage(consoleInventory(m))
		case "redraw":
			s.terminalMu.Lock()
			s.mu.Lock()
			if s.active == p {
				pty.RequestRedraw(s.terminal)
			}
			s.mu.Unlock()
			s.terminalMu.Unlock()
			err = p.controlMessage(message{Type: "redrawn"})
		default:
			err = extra(m)
		}
		if err != nil {
			return err
		}
	}
}

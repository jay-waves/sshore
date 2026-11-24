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
	parent          *shellTask
	ownerClaimed    bool
	scopeLost       bool
	pinned          bool
	pinUntil        time.Time
	pinTimer        *time.Timer
}

func (s *shellTask) taskName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record.Name
}

func (s *shellTask) expired() bool {
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
		return identityFromUID(s.record.UID)
	}
	return s.record.identity()
}

// Call with mu held.
func (s *shellTask) finishLocked() {
	s.once.Do(func() {
		if s.pinTimer != nil {
			s.pinTimer.Stop()
		}
		saveShellTree(s.treeIdentity(), s.record.Name, nil, taskExited)
		close(s.done)
	})
}

func (s *shellTask) close() {
	s.once.Do(func() {
		s.mu.Lock()
		if s.pinTimer != nil {
			s.pinTimer.Stop()
		}
		terminal, attachment := s.terminal, s.endTerminal()
		identity, name := s.treeIdentity(), s.record.Name
		s.mu.Unlock()
		if attachment != nil {
			attachment.Close()
		}
		if terminal != nil {
			_ = terminal.Close()
		}
		saveShellTree(identity, name, nil, taskExited)
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
	if s.exited {
		s.mu.Unlock()
		return nil
	}
	if s.killing {
		s.mu.Unlock()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-s.done:
			return nil
		case <-timer.C:
			return fmt.Errorf("shell has not finished stopping")
		}
	}
	s.killing, s.exitReason = true, reason
	s.mu.Unlock()
	return s.stopTerminal()
}

// The caller claims shutdown under mu before stopping the validated terminal.
func (s *shellTask) stopTerminal() error {
	s.mu.Lock()
	terminal, process := s.terminal, s.process
	s.mu.Unlock()
	var err error
	if terminal != nil {
		if s.hosted {
			err = stopHostedTerminal(process.PID, process.Start)
		} else {
			err = terminal.Stop()
		}
	}
	if err != nil {
		s.mu.Lock()
		s.killing = false
		s.mu.Unlock()
		return err
	}
	if !s.hosted {
		s.close()
		return nil
	}
	s.mu.Lock()
	if terminal == nil {
		s.endTerminal()
	}
	s.finishLocked()
	s.mu.Unlock()
	return nil
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
		return taskUnknown
	case s.active != nil:
		return taskActive
	case s.process.PID == 0:
		return taskStarting
	default:
		return taskDetached
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
	if err := s.tasks.checkNameLocked(s.record.UID, name); err != nil {
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
	case "shell_owner":
		return true, s.serveOwner(c, m)
	case "detach":
		if err := checkPeerVersion(m.SoftwareVersion, "client"); err != nil {
			return true, writeMessage(c, message{Type: "error", Error: err.Error()})
		}
		s.mu.Lock()
		p := s.active
		var err error
		if p == nil {
			err = fmt.Errorf("shell has no active attachment")
		} else {
			err = s.pinLocked(m.TTL)
		}
		reply = message{Type: "pinned", PinUntil: s.pinUntil}
		s.mu.Unlock()
		if err == nil {
			err = p.controlMessage(message{Type: "detach_requested"})
		}
		if err != nil {
			reply = message{Type: "error", Error: err.Error()}
		}
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
func (s *shellTask) attachManaged(p *attachment, m message, start func(message) error) error {
	if err := validateAttach(m); err != nil {
		return err
	}
	s.terminalMu.Lock()
	s.mu.Lock()
	s.reapUnretainedLocked()
	initial := s.terminal == nil
	if s.killing || s.hosted && (m.Instance != "" && m.Instance != s.record.UID || initial && m.Action == "existing") {
		s.mu.Unlock()
		s.terminalMu.Unlock()
		return fmt.Errorf("shell changed or is unavailable; refresh with sshore list")
	}
	err := s.attachTerminal(p, m, start, func() error {
		if initial && m.Pinned {
			if err := s.pinLocked(0); err != nil {
				return err
			}
		}
		instance := s.record.UID
		if !s.hosted {
			instance = ""
		}
		tree := s.treeIdentity()
		saveShellTree(tree, s.record.Name, m.Tree, taskActive, m.SourceHost, m.SourceMachineID)
		return p.controlMessage(message{Type: "ready", Tree: shellTreeFor(tree), Pinned: s.pinned, PinUntil: s.pinUntil, Instance: instance, PID: s.process.PID, Name: s.record.Name})
	})
	failedInitial := err != nil && initial && !errors.Is(err, errShellAttached) && !errors.Is(err, errShellExited)
	if failedInitial && s.hosted && s.terminal != nil {
		s.endTerminal()
		s.exitReason = "initial attachment failed"
		_ = s.terminal.Close()
		s.finishLocked()
	} else if failedInitial && !s.hosted {
		s.killing, s.exitReason = true, "initial attachment failed"
		s.endTerminal()
	}
	s.mu.Unlock()
	s.terminalMu.Unlock()
	if failedInitial && !s.hosted {
		_ = s.stopTerminal()
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
	env := append(append([]string(nil), source...), "TERM="+term, "SSHORE_SOCKET="+instance, "SSHORE_TREE_ID="+identityFromUID(instance))
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
		s.reapUnretainedLocked()
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
			if m.Action != "switch" {
				s.mu.Lock()
				err := s.pinLocked(m.TTL)
				s.mu.Unlock()
				if err != nil {
					if err := p.controlMessage(message{Type: "pin_failed", Error: err.Error()}); err != nil {
						return err
					}
					continue
				}
			}
			p.approvals.disable()
			released = true
			name, err := release()
			if err == nil && name != "" {
				s.mu.Lock()
				reply := message{Type: "detached", Name: name, Pinned: s.pinned, PinUntil: s.pinUntil}
				s.mu.Unlock()
				return p.FinishDetach(reply)
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
			s.mu.Lock()
			current := s.treeIdentity()
			s.mu.Unlock()
			err = p.controlMessage(consoleInventory(m, current))
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

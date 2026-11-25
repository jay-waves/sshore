package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/term"
	"github.com/jay-waves/sshore/internal/termui"
)

type shellRegistry interface {
	attachmentTargets(parent, ref string, hosted bool) ([]sessionTarget, error)
	renameShell(shell *shellTask, old, name, instance string) error
	childShells(*shellTask) []*shellTask
	isShuttingDown() bool
}

// A daemon-owned shell with one terminal and at most one attachment. Entry
// points supply owner supervision and their additional commands.
type shellTask struct {
	endpoints  terminalEndpoints
	mu         sync.Mutex // Protect shell state and attachment ownership.
	terminalMu sync.Mutex // Serialize replay and redraw with live output. Acquire before mu.
	nameMu     sync.Mutex // Read the registry label without waiting for terminal I/O.

	terminal                      *pty.Terminal
	pid                           int
	shellName                     string
	reportedDirectory             string
	modes                         term.InputState
	active                        *attachment
	exited                        bool
	registry                      shellRegistry
	forest                        *shellForest
	log                           io.Writer
	cgroup                        string
	cgroupErr                     error
	record                        localShellRecord
	environment                   []string
	done                          chan struct{}
	once                          sync.Once
	hosted, killing               bool
	started                       time.Time
	exitReason                    string
	parent                        *shellTask
	treeEnded                     atomic.Bool // Published by the registry's forest lock.
	ownerClaimed                  bool
	remoteOwner, remoteOwnerToken string
	remoteChildren                map[*remoteShellChild]struct{}
	scopeLost                     bool
	pinned                        bool
	pinUntil                      time.Time
	pinTimer                      *time.Timer
}

func (s *shellTask) managedShell() *shellTask { return s }

func (s *shellTask) setInitialIdentity(uid, name string) {
	s.record.UID, s.record.Name = uid, name
}

func (s *shellTask) taskName() string {
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
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

func currentShellDirectory(pid int, reported string) string {
	if reported != "" || pid <= 0 {
		return reported
	}
	directory, err := sys.WorkingDirectory(pid)
	if err != nil {
		return ""
	}
	return directory
}

// Call with mu held. Use the startup directory when the shell's location is unavailable.
func (s *shellTask) directoryOrStartupLocked() string {
	return cmp.Or(currentShellDirectory(s.pid, s.reportedDirectory), s.record.Directory)
}

func (s *shellTask) treeIdentity() string {
	if s.hosted || validTaskUID(s.record.UID) {
		return identityFromUID(s.record.UID)
	}
	return (localShellRecord{Token: s.record.Token}).identity()
}

// Call with mu held.
func (s *shellTask) finishLocked() {
	s.once.Do(func() {
		if s.pinTimer != nil {
			s.pinTimer.Stop()
		}
		s.forest.end(s)
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
		s.mu.Unlock()
		if attachment != nil {
			attachment.Close()
		}
		if terminal != nil {
			_ = terminal.Close()
		}
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
	terminal := s.terminal
	s.mu.Unlock()
	var err error
	if terminal != nil {
		err = terminal.Stop()
	}
	if err != nil {
		s.mu.Lock()
		s.killing = false
		s.mu.Unlock()
		return err
	}
	if terminal == nil {
		s.close()
		return nil
	}
	// The PTY pump reports the exit before closing the task and SSH lease.
	timer := time.NewTimer(requestTimeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("shell process ended, but terminal cleanup timed out")
	}
}

// Call with mu held.
func (s *shellTask) state() string {
	switch {
	case s.exited:
		return taskExited
	case s.active != nil:
		return taskActive
	case s.pid == 0:
		return taskStarting
	case s.forest.waiting(s):
		return taskWaiting
	default:
		return taskDetached
	}
}

func (s *shellTask) serveCommand(c net.Conn, m message, extra func(*attachment, message) error, status func(string) message) (bool, error) {
	var reply message
	switch m.Type {
	case "tree_hold":
		return true, s.holdTree(c, m)
	case "terminal_open":
		return true, s.endpoints.serve(c, m)
	case "attach_control":
		if err := checkPeerVersion(m.SoftwareVersion, "client"); err != nil {
			return true, writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		}
		if err := writeMessage(c, message{Type: "attach_control_ready", SoftwareVersion: version}); err != nil {
			return true, err
		}
		return true, s.attach(c, extra)
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
				err = s.registry.renameShell(s, m.Action, m.Name, m.Instance)
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

var (
	errShellExited   = errors.New("session is exiting")
	errShellAttached = errors.New("session already attached")
)

// Call with mu held.
func (s *shellTask) endTerminal() *attachment {
	p := s.active
	s.active, s.exited = nil, true
	s.forest.end(s)
	return p
}

func shellSize(m message) pty.Size {
	return pty.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}
}

func (s *shellTask) pumpTerminal(t *pty.Terminal) {
	buf := make([]byte, 8192)
	var directoryReports term.DirectoryReport
	for {
		n, err := t.Read(buf)
		if n > 0 {
			s.terminalMu.Lock()
			var directory string
			if runtime.GOOS == "windows" && !s.hosted {
				directory = directoryReports.Observe(&s.modes, buf[:n])
			} else {
				s.modes.Observe(buf[:n])
			}
			// Keep live output ordered without holding the shell lock during I/O.
			s.mu.Lock()
			if directory != "" {
				s.reportedDirectory = directory
			}
			p := s.active
			s.mu.Unlock()
			if p != nil {
				_ = p.Output(buf[:n])
			}
			s.terminalMu.Unlock()
		}
		if err != nil {
			break
		}
	}
	s.terminalExited(t.Wait())
}

func (s *shellTask) terminalExited(err error) {
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
	s.mu.Lock()
	p := s.endTerminal()
	reason := s.exitReason
	if reason == "" {
		reason = "shell exited"
		if s.hosted {
			reason = fmt.Sprintf("shell exited (code %d)", code)
			if e, ok := errors.AsType[*exec.ExitError](err); ok {
				if status, ok := e.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					reason = "shell terminated by " + status.Signal().String()
				}
			}
		} else if err != nil {
			reason = err.Error()
		}
		s.exitReason = reason
	}
	s.mu.Unlock()
	termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Info, "Shell exited (code %d): %s", code, cleanText(reason))
	if p != nil {
		p.finishExit(message{
			Type: "exit", Code: code, Reason: reason,
			DaemonShutdown: s.registry.isShuttingDown(),
			RetainedShells: s.retainedChildren(),
		})
	}
	if !s.hosted {
		s.close()
	} else {
		s.mu.Lock()
		s.finishLocked()
		s.mu.Unlock()
	}
}

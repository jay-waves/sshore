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
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/tmux"
)

// tmuxTask binds the shared terminal state to daemon registration and routing.
type tmuxTask struct {
	hostedState
	handoff    *localHandoff
	endpoints  terminalEndpoints
	mu         sync.Mutex // Protect shell state and attachment ownership.
	runtime    tmux.Task[*attachment, *tmuxTask]
	terminalMu sync.Mutex // Serialize replay and redraw with live output. Acquire before mu.

	shellName                     string
	programLine                   string
	program                       bool
	reportedDirectory             string
	registry                      *daemonTasks
	log                           io.Writer
	cgroup                        string
	cgroupErr                     error
	record                        localShellRecord // Immutable after registration.
	environment                   []string
	hosted, killing               bool
	exitReason                    string
	ownerClaimed                  bool
	remoteOwner, remoteOwnerToken string
	remoteChildren                map[*shellTarget]struct{}
}

func (s *tmuxTask) setInitialIdentity(uid, name string) {
	s.record.UID, s.record.Name = uid, name
	s.runtime.SetIdentity(s, taskShortCode(s.treeIdentity()))
}

func (s *tmuxTask) taskName() string { return s.record.Name }

func (s *tmuxTask) expired() bool {
	select {
	case <-s.runtime.Done():
		return true
	default:
		return false
	}
}

func (s *tmuxTask) inventoryView() shellView {
	if !s.mu.TryLock() {
		return shellView{Instance: s.record.UID, Name: s.record.Name, State: taskUnknown}
	}
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// snapshotLocked projects the managed terminal state for either ingress.
func (s *tmuxTask) snapshotLocked() shellView {
	pinned, until := s.runtime.PinState()
	view := shellView{Owner: s.parentIdentityLocked(), Pinned: pinned, PinUntil: until,
		Instance: s.record.UID, Name: s.record.Name, PID: s.runtime.PID(), Shell: s.shellName,
		Program: s.program, ProgramLine: s.programLine, State: s.state(),
		Directory: cmp.Or(s.reportedDirectory, s.record.Directory)}
	if active := s.runtime.Active(); active != nil {
		view.SourceHost = active.sourceHost
	}
	return view
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
func (s *tmuxTask) directoryOrStartupLocked() string {
	return cmp.Or(currentShellDirectory(s.runtime.PID(), s.reportedDirectory), s.record.Directory)
}

func (s *tmuxTask) treeIdentity() string {
	if s.hosted || validTaskUID(s.record.UID) {
		return identityFromUID(s.record.UID)
	}
	return (localShellRecord{Token: s.record.Token}).identity()
}

func (s *tmuxTask) expireUnstarted(after time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime.ExpireUnstarted(after, func() {
		s.mu.Lock()
		if s.runtime.Started() {
			s.mu.Unlock()
			return
		}
		p, ok := s.runtime.BeginClose()
		s.mu.Unlock()
		if ok {
			s.completeClose(p)
		}
	})
}

func (s *tmuxTask) close() {
	s.mu.Lock()
	p, ok := s.runtime.BeginClose()
	s.mu.Unlock()
	if ok {
		s.completeClose(p)
	}
}

func (s *tmuxTask) completeClose(p *attachment) {
	if p != nil {
		p.Close()
	}
	s.runtime.CompleteClose()
}

func (s *tmuxTask) stopTask() error {
	return s.kill(s.record.UID, "killed by sshore kill")
}

func (s *tmuxTask) kill(instance, reason string) error {
	s.mu.Lock()
	if s.hosted && (instance == "" || instance != s.record.UID) {
		s.mu.Unlock()
		return fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	if s.runtime.Exited() || s.killing {
		s.mu.Unlock()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-s.runtime.Done():
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
func (s *tmuxTask) stopTerminal() error {
	s.mu.Lock()
	started := s.runtime.Started()
	s.mu.Unlock()
	if !started {
		s.close()
		return nil
	}
	if err := s.runtime.Stop(); err != nil {
		s.mu.Lock()
		s.killing = false
		s.mu.Unlock()
		return err
	}
	// The PTY pump reports the exit before closing the task and SSH lease.
	timer := time.NewTimer(requestTimeout)
	defer timer.Stop()
	select {
	case <-s.runtime.Done():
		return nil
	case <-timer.C:
		return fmt.Errorf("shell process ended, but terminal cleanup timed out")
	}
}

// Call with mu held.
func (s *tmuxTask) state() string {
	switch {
	case s.runtime.Exited():
		return taskExited
	case s.runtime.Active() != nil:
		return taskActive
	case !s.runtime.Started():
		return taskStarting
	case s.runtime.Waiting():
		return taskWaiting
	default:
		return taskDetached
	}
}

func (s *tmuxTask) serveTask(c net.Conn, m message) error {
	if s.hosted {
		return s.serveHostedIngress(c, m)
	}
	return s.serveLocalTask(c, m)
}

func (s *tmuxTask) serveCommand(c net.Conn, m message, extra func(*attachment, message) error) (bool, error) {
	var reply message
	switch m.Type {
	case "tree_hold":
		return true, s.holdTree(c, m)
	case "terminal_open":
		return true, s.endpoints.serve(c, m)
	case "attach":
		if err := checkPeerVersion(m.SoftwareVersion, "client"); err != nil {
			return true, writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		}
		return true, s.attach(c, m, extra)
	case "status":
		s.mu.Lock()
		if s.hosted {
			view := s.snapshotLocked()
			reply = message{Type: "session", Owner: view.Owner, Action: view.State, Pinned: view.Pinned, PinUntil: view.PinUntil, Instance: view.Instance, Name: view.Name, SoftwareVersion: version}
		} else {
			pinned, until := s.runtime.PinState()
			reply = message{Type: "session", Pinned: pinned, PinUntil: until, Name: s.record.Name, SoftwareVersion: s.record.Version}
		}
		s.mu.Unlock()
	case "shell_owner":
		return true, s.serveOwner(c, m)
	case "detach":
		if err := checkPeerVersion(m.SoftwareVersion, "client"); err != nil {
			return true, writeMessage(c, message{Type: "error", Error: err.Error()})
		}
		s.mu.Lock()
		p := s.runtime.Active()
		var err error
		if p == nil {
			err = fmt.Errorf("shell has no active attachment")
		} else {
			err = s.pinLocked(time.Duration(m.TTLNanos))
		}
		_, until := s.runtime.PinState()
		reply = message{Type: "pinned", PinUntil: until}
		s.mu.Unlock()
		if err == nil {
			err = p.controlMessage(message{Type: "detach_requested"})
		}
		if err != nil {
			reply = message{Type: "error", Error: err.Error()}
		}
	case "kill":
		var err error
		if s.hosted {
			err = checkPeerVersion(m.SoftwareVersion, "client")
		}
		if err == nil {
			err = s.kill(m.Instance, "killed by sshore kill")
			reply.Type = "killed"
		}
		if err != nil {
			reply = message{Type: "error", Error: err.Error()}
		}
	default:
		return false, nil
	}
	return true, writeMessage(c, reply)
}

// Call with mu held.
func shellSize(m message) pty.Size {
	return pty.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}
}

func (s *tmuxTask) pumpTerminal() {
	err := s.runtime.PumpOutput(func(data []byte) {
		s.terminalMu.Lock()
		defer s.terminalMu.Unlock()
		directory := s.runtime.Observe(data, runtime.GOOS == "windows" && !s.hosted)
		// Keep live output ordered without holding the shell lock during I/O.
		s.mu.Lock()
		if directory != "" {
			s.reportedDirectory = directory
		}
		p := s.runtime.Active()
		s.mu.Unlock()
		if p != nil {
			_ = p.Output(data)
		}
	})
	s.terminalExited(err)
}

func (s *tmuxTask) terminalExited(err error) {
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
	p, ok := s.runtime.BeginClose()
	if !ok {
		s.mu.Unlock()
		return
	}
	defer s.completeClose(p)
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
}

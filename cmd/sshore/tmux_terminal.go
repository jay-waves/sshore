package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
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
	handoff *localHandoff
	mu      sync.Mutex // Protect shell state and attachment ownership.
	runtime *tmux.Task[*attachment, *tmuxTask]

	registry       *daemonTasks
	log            io.Writer
	cgroup         string
	cgroupErr      error
	record         localShellRecord // Immutable after registration.
	environment    []string
	hosted         bool
	remoteChildren map[*shellTarget]struct{}
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

func (s *tmuxTask) snapshot() shellView {
	state := s.runtime.Snapshot()
	view := shellView{Owner: taskKey(state.OwnerID), Pinned: state.Pinned, PinUntil: state.PinUntil,
		Instance: state.ID, Name: state.Name, PID: state.PID, Shell: state.Process.Shell,
		Program: state.Process.ProgramLine != "", ProgramLine: state.Process.ProgramLine, State: state.State,
		DetachedAt: state.DetachedAt, Stopping: state.Stopping, RemoteOwner: state.RemoteOwner,
		Directory: cmp.Or(state.Directory, s.record.Directory)}
	if active := state.Active; active != nil {
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
	return cmp.Or(currentShellDirectory(s.runtime.PID(), s.runtime.Directory()), s.record.Directory)
}

func (s *tmuxTask) treeIdentity() string {
	if s.hosted || validTaskUID(s.record.UID) {
		return taskKey(s.record.UID)
	}
	return (localShellRecord{Token: s.record.Token}).identity()
}

func (s *tmuxTask) stopTask() error {
	return s.kill(s.record.UID, "killed by sshore kill")
}

func (s *tmuxTask) kill(instance, reason string) error {
	if s.hosted && (instance == "" || instance != s.record.UID) {
		return fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	return s.runtime.Stop(reason)
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
		if err := checkPeerVersion(m.SoftwareVersion, "terminal client"); err != nil {
			return true, err
		}
		return true, s.runtime.OfferTerminal(c, func(token string) error {
			return writeMessage(c, message{Type: "terminal_ready", SoftwareVersion: version, Token: token})
		})
	case "attach":
		if err := checkPeerVersion(m.SoftwareVersion, "client"); err != nil {
			return true, writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		}
		return true, s.attach(c, m, extra)
	case "status":
		view := s.snapshot()
		if s.hosted {
			reply = message{Type: "session", Owner: view.Owner, Action: view.State, Pinned: view.Pinned, PinUntil: view.PinUntil, Instance: view.Instance, Name: view.Name, SoftwareVersion: version}
		} else {
			reply = message{Type: "session", Pinned: view.Pinned, PinUntil: view.PinUntil, Name: s.record.Name, SoftwareVersion: s.record.Version}
		}
	case "shell_owner":
		return true, s.serveOwner(c, m)
	case "detach":
		if err := checkPeerVersion(m.SoftwareVersion, "client"); err != nil {
			return true, writeMessage(c, message{Type: "error", Error: err.Error()})
		}
		p, until, err := s.runtime.RequestDetach(time.Duration(m.TTLNanos))
		reply = message{Type: "pinned", PinUntil: until}
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

func (s *tmuxTask) reportTerminalExit(p *attachment, exit tmux.Exit) {
	reason := exit.Reason
	if reason == "" {
		reason = "shell exited"
		if s.hosted {
			reason = fmt.Sprintf("shell exited (code %d)", exit.Code)
			if e, ok := errors.AsType[*exec.ExitError](exit.Err); ok {
				if status, ok := e.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					reason = "shell terminated by " + status.Signal().String()
				}
			}
		} else if exit.Err != nil {
			reason = exit.Err.Error()
		}
	}
	termui.Log(s.log, canonicalTaskCode(s.treeIdentity()), termui.Info, "Shell exited (code %d): %s", exit.Code, cleanText(reason))
	if p != nil {
		p.finishExit(message{
			Type: "exit", Code: exit.Code, Reason: reason,
			DaemonShutdown: s.registry.isShuttingDown(),
			RetainedShells: s.retainedChildren(),
		})
	}
}

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/tmux"
)

// shellTask is the daemon endpoint for a terminal session.
type shellTask struct {
	hostedState
	handoff *localHandoff
	mu      sync.Mutex // Protect daemon routes, protocol streams and approvals.
	session *tmux.Session

	registry    *daemonTasks
	log         io.Writer
	record      localShellRecord // Immutable after registration.
	environment []string
	hosted      bool
	frontends   map[*tmux.Attachment]*attachment // Keep protocol state through detach acknowledgement.
	terminals   map[string]*pendingTerminal
}

func (s *shellTask) taskName() string { return s.record.Name }

func (s *shellTask) snapshot() shellView {
	state := s.session.Snapshot()
	view := shellView{Owner: taskKey(state.OwnerID), Pinned: state.Pinned, PinUntil: state.PinUntil,
		Instance: state.ID, Name: state.Name, PID: state.PID, Shell: state.Process.Shell,
		Program: state.Process.ProgramLine != "", ProgramLine: state.Process.ProgramLine, State: state.State,
		DetachedAt: state.DetachedAt, Stopping: state.Stopping,
		Directory: state.Directory}
	if active := s.frontend(state.Active); active != nil {
		view.SourceHost = active.sourceHost
	}
	return view
}

func (s *shellTask) treeIdentity() string {
	if s.hosted || validTaskUID(s.record.UID) {
		return taskKey(s.record.UID)
	}
	return (localShellRecord{Token: s.record.Token}).identity()
}

func (s *shellTask) stopTask() error {
	return s.kill(s.record.UID, "killed by sshore kill")
}

func (s *shellTask) kill(instance, reason string) error {
	if s.hosted && (instance == "" || instance != s.record.UID) {
		return fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	return s.session.Stop(reason)
}

func (s *shellTask) serveTask(c net.Conn, m message) error {
	if s.hosted {
		return s.serveHostedIngress(c, m)
	}
	return s.serveLocalTask(c, m)
}

func (s *shellTask) serveCommand(c net.Conn, m message, extra func(*attachment, message) error) (bool, error) {
	var reply message
	switch m.Type {
	case "tree_hold":
		return true, s.holdTree(c, m)
	case "terminal_open":
		if err := checkPeerVersion(m.SoftwareVersion, "terminal client"); err != nil {
			return true, err
		}
		return true, s.offerTerminal(c)
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
		active, until, err := s.session.RequestDetach(time.Duration(m.TTLNanos))
		reply = message{Type: "pinned", PinUntil: until}
		if err == nil {
			if frontend := s.frontend(active); frontend != nil {
				err = frontend.controlMessage(message{Type: "detach_requested"})
			} else {
				err = fmt.Errorf("shell has no active attachment")
			}
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

func shellSize(m message) pty.Size {
	return pty.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}
}

func (s *shellTask) reportTerminalExit(active *tmux.Attachment, exit tmux.Exit) {
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
	if p := s.frontend(active); p != nil {
		p.finishExit(message{
			Type: "exit", Code: exit.Code, Reason: reason,
			DaemonShutdown: s.registry.isShuttingDown(),
			RetainedShells: s.retainedChildren(),
		})
	}
}

//go:build linux || windows

package main

import (
	"cmp"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

type remoteShell struct {
	tmuxTask
	hostedRoutes
	resourceApproval resourceApproval
}

func (s *remoteShell) serveHostedTask(c net.Conn, m message, relay func(net.Conn, message) error) (bool, error) {
	if handled, err := s.serveCommand(c, m, s.attachmentCommand, s.status); handled {
		return true, err
	}
	switch m.Type {
	case "route_mount", "route_forward", "route_files":
		s.serveRoutedResource(c, m, relay)
	case "approval_request":
		if err := s.requestApproval(c, m); err != nil {
			return true, writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
		}
	default:
		return false, nil
	}
	return true, nil
}

func (s *remoteShell) hostedInventoryView() shellView {
	if !s.mu.TryLock() {
		return shellView{Instance: s.record.UID, Name: s.record.Name, State: taskUnknown}
	}
	defer s.mu.Unlock()
	return s.hostedSnapshotLocked()
}

func (s *remoteShell) hostedSnapshotLocked() shellView {
	pinned, until := s.runtime.PinState()
	view := shellView{
		Owner: s.parentIdentityLocked(), Pinned: pinned, PinUntil: until,
		Instance: s.record.UID, Name: s.record.Name,
		PID: s.runtime.PID(), Shell: s.shellName, Program: s.program, ProgramLine: s.programLine, State: s.state(),
	}
	if active := s.runtime.Active(); active != nil {
		view.SourceHost = active.sourceHost
	}
	view.Directory = cmp.Or(s.reportedDirectory, s.record.Directory)
	return view
}

func (s *remoteShell) status() message {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := s.hostedSnapshotLocked()
	return message{Type: "session", Owner: view.Owner, Action: view.State, Pinned: view.Pinned, PinUntil: view.PinUntil, Instance: view.Instance, Name: view.Name, SoftwareVersion: version}
}
func (tasks *daemonTasks) registerHostedShell(req hostedShellRequest) (*remoteShell, error) {
	s := &remoteShell{
		tmuxTask: tasks.newTmuxTask(localShellRecord{Name: req.Name, Directory: req.Directory}, req.Env, req.parent, true),
	}
	if err := tasks.register("", s); err != nil {
		if errors.Is(err, errObjectNameInUse) {
			return nil, fmt.Errorf("name %s already exists; use a different shell name", req.Name)
		}
		return nil, err
	}
	s.watchParent(req.parent)
	// A failed initial attach should not leave an empty shell indefinitely.
	s.expireUnstarted(30 * time.Second)
	return s, nil
}

func resolveHostedShell(name, instance string) (selectedTask, error) {
	if err := validSession(name); err != nil {
		return selectedTask{}, err
	}
	rows, err := queryRegisteredTasks(taskQuery{Kind: "hosted", Ref: name, UID: instance})
	if err != nil || len(rows) == 0 {
		return selectedTask{}, err
	}
	return rows[0], nil
}

func registerPlatformDaemonRoutes(mux *http.ServeMux, tasks *daemonTasks) {
	mux.HandleFunc("POST /shells", func(w http.ResponseWriter, r *http.Request) {
		var req hostedShellRequest
		if !readDaemonBody(w, r, &req) {
			return
		}
		if !filepath.IsAbs(req.Directory) {
			http.Error(w, "invalid shell creation", 400)
			return
		}
		shell, err := tasks.registerHostedShell(req)
		if err != nil {
			status := http.StatusConflict
			if errors.Is(err, errDaemonDraining) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), status)
			return
		}
		_ = json.MarshalWrite(w, sessionTarget{Instance: shell.record.UID, Name: shell.taskName()})
	})
}

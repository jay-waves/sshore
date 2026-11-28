//go:build linux || windows

package main

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

type hostedState struct {
	hostedRoutes
	resourceApproval resourceApproval
}

func (s *tmuxTask) serveHostedTask(c net.Conn, m message, relay func(net.Conn, message) error) (bool, error) {
	if handled, err := s.serveCommand(c, m, s.hostedAttachmentCommand); handled {
		return true, err
	}
	switch m.Type {
	case "route_resource":
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

func (tasks *daemonTasks) registerHostedShell(req hostedShellRequest) (*tmuxTask, error) {
	s := tasks.newTmuxTask(localShellRecord{Name: req.Name, Directory: req.Directory}, req.Env, req.parent, true)
	if err := tasks.registerShell(s, req.parent); err != nil {
		if errors.Is(err, errObjectNameInUse) {
			return nil, fmt.Errorf("name %s already exists; use a different shell name", req.Name)
		}
		return nil, err
	}
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

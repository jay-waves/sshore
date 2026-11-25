//go:build linux || windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func (s *remoteShell) status(detail string) message {
	view := s.hostedView(detail)
	return message{Type: "session", Owner: view.Owner, Action: view.State, Pinned: view.Pinned, PinUntil: view.PinUntil, Instance: view.Instance, Name: view.Name, SoftwareVersion: version,
		Attached: view.State == taskActive, PID: view.PID, AgeSeconds: view.Age, Foreground: view.Foreground, Directory: view.Directory}
}
func (tasks *daemonTasks) registerHostedShell(req hostedShellRequest) (*remoteShell, error) {
	s := &remoteShell{
		shellTask: tasks.newShellTask(localShellRecord{Name: req.Name, Directory: req.Directory}, req.Env, req.parent, true),
		tasks:     tasks,
	}
	if err := tasks.register("", s); err != nil {
		if errors.Is(err, errObjectNameInUse) {
			return nil, fmt.Errorf("name %s already exists; use a different shell name", req.Name)
		}
		return nil, err
	}
	s.watchParent(req.parent)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		defer stop()
		s.watchLifetime(ctx)
	}()
	return s, nil
}

func (s *remoteShell) watchLifetime(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.terminal != nil {
			_ = s.terminal.Close()
		}
	}()
	// A failed initial attach should not leave an empty shell indefinitely.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	initialAttachDeadline := timer.C
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.endTerminal()
			s.finishLocked()
			s.mu.Unlock()
			return
		case <-s.done:
			return
		case <-initialAttachDeadline:
			s.mu.Lock()
			empty := s.terminal == nil
			if empty {
				s.endTerminal()
				s.finishLocked()
			}
			s.mu.Unlock()
			if empty {
				return
			}
			initialAttachDeadline = nil
		}
	}
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
		_ = json.NewEncoder(w).Encode(sessionTarget{Instance: shell.record.UID, Name: shell.taskName()})
	})
}

//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
)

func (tasks *daemonTasks) hostedViews(detail string) []shellView {
	shells := liveTasks[*remoteShell](tasks)
	diagnostics := detail == "" || detail == "sessions"
	var views []shellView
	var groups []int
	for _, shell := range shells {
		view, group := shell.view(detail)
		views = append(views, view)
		if diagnostics {
			groups = append(groups, group)
		}
	}
	if diagnostics && len(views) > 0 {
		snapshot := newProcessSnapshot(proc.ReadProcesses())
		for i, group := range groups {
			views[i].Foreground = snapshot.foregroundPrograms(group)
			if detail == "sessions" {
				views[i].ProcessTree = snapshot.sessionTree(views[i].PID)
			}
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Instance < views[j].Instance })
	return views
}

func hostedShells(detail string) ([]shellView, error) {
	if active := accountActivity.Load(); active != nil && active.tasks != nil {
		return active.tasks.hostedViews(detail), nil
	}
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if detail == "health" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	var views []shellView
	err = daemonJSON(ctx, record, "/tasks/hosted?detail="+detail, nil, &views)
	return views, err
}

func (tasks *daemonTasks) resolveHostedShell(name, instance string) (shellView, error) {
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	now := time.Now()
	prefix := shortTaskPrefix(name)
	id := instance
	if id == "" && validTaskUID(prefix) {
		id, prefix = prefix, ""
	}
	if id != "" {
		if shell, ok := tasks.lookupLocked(id, now).(*remoteShell); ok {
			view, _ := shell.view("health")
			return view, nil
		}
		if instance != "" {
			return shellView{}, nil
		}
	}
	// A full hexadecimal reference with no matching ID may be an existing alias.
	var found *remoteShell
	for id, task := range tasks.liveLocked(now) {
		shell, ok := task.(*remoteShell)
		if !ok || (prefix == "" || !strings.HasPrefix(id, prefix)) && shell.taskName() != name {
			continue
		}
		if found != nil {
			return shellView{}, fmt.Errorf("ambiguous shell name %s", name)
		}
		found = shell
	}
	if found == nil {
		return shellView{}, nil
	}
	view, _ := found.view("health")
	return view, nil
}

func dialHostedTask(ctx context.Context, ref string, timeout time.Duration) (net.Conn, error) {
	r, err := existingDaemonRecord()
	if err != nil {
		return nil, err
	}
	if !validTaskUID(ref) {
		return nil, fmt.Errorf("invalid task ID")
	}
	r.UID = ref
	return dialLocalSessionContext(ctx, r, time.Now().Add(timeout))
}

type hostedShellRequest struct {
	Runtime, Directory, Name, UID string
	Env                           []string
	Temporary                     bool
}

func registerPlatformDaemonRoutes(mux *http.ServeMux, tasks *daemonTasks) {
	mux.HandleFunc("POST /tasks/hosted/resolve", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		name := query.Get("name")
		if err := validSession(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		view, err := tasks.resolveHostedShell(name, query.Get("instance"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(view)
	})
	mux.HandleFunc("POST /tasks/hosted", func(w http.ResponseWriter, r *http.Request) {
		detail := r.URL.Query().Get("detail")
		if detail != "" && detail != "health" && detail != "inventory" && detail != "sessions" {
			http.Error(w, "invalid shell inventory detail", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(tasks.hostedViews(detail))
	})
	mux.HandleFunc("POST /shells", func(w http.ResponseWriter, r *http.Request) {
		var req hostedShellRequest
		if !readDaemonBody(w, r, &req) {
			return
		}
		if !validTaskUID(req.UID) || validSession(req.Name) != nil || !filepath.IsAbs(req.Directory) || !filepath.IsAbs(req.Runtime) {
			http.Error(w, "invalid shell creation", 400)
			return
		}
		if err := privateStatePath(req.Runtime, true); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := tasks.registerHostedShell(req); err != nil {
			status := http.StatusConflict
			if errors.Is(err, errDaemonDraining) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
func createRemoteShell(executable string, id taskIdentitySelection, temporary bool, cwd string, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	record, err := ensureLocalDaemonContext(ctx, executable)
	if err != nil {
		return err
	}
	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	req := hostedShellRequest{Runtime: dir, Directory: cwd, Name: id.name, UID: id.uid, Env: env, Temporary: temporary}
	return daemonJSON(ctx, record, "/shells", req, nil)
}

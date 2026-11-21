//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
)

func hostedDiagnostics(views []shellView, groups []int, detail string) {
	if detail != "health" {
		for i, group := range groups {
			views[i].Directory = cleanText(sessionDirectory(group, views[i].PID))
		}
	}
	if len(views) != 0 && (detail == "" || detail == "sessions") {
		snapshot := newProcessSnapshot(proc.ReadProcesses())
		for i, group := range groups {
			views[i].Foreground = snapshot.foregroundPrograms(group)
			if detail == "sessions" {
				views[i].ProcessTree = snapshot.sessionTree(views[i].PID)
			}
		}
	}
	slices.SortFunc(views, func(a, b shellView) int { return strings.Compare(a.Instance, b.Instance) })
}

func hostedShells(detail string) ([]shellView, error) {
	request := taskInventoryRequest{Hosted: true, Detail: detail}
	if a := accountActivity.Load(); a != nil && a.tasks != nil {
		snapshot, _ := a.tasks.taskInventory(request)
		return snapshot.Shells, nil
	}
	snapshot, _, err := readTaskInventory(request)
	return snapshot.Shells, err
}

func (tasks *daemonTasks) resolveHostedShell(name, instance string) (shellView, error) {
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	now := time.Now()
	prefix := ""
	if isTaskIDPrefix(name) {
		prefix = strings.ToLower(name)
	}
	id := instance
	if id == "" && validTaskUID(prefix) {
		id, prefix = prefix, ""
	}
	if id != "" {
		if shell, ok := tasks.lookupLocked(id, now).(*remoteShell); ok {
			view, _ := shell.hostedView("health")
			return view, nil
		}
		if instance != "" {
			return shellView{}, nil
		}
	}
	// A full hexadecimal reference with no matching ID may be an existing alias.
	var found, alias *remoteShell
	for id, task := range tasks.liveLocked(now) {
		shell, ok := task.(*remoteShell)
		if !ok {
			continue
		}
		if prefix != "" && strings.HasPrefix(id, prefix) {
			if found != nil {
				return shellView{}, fmt.Errorf("ambiguous shell reference %s", name)
			}
			found = shell
		}
		if shell.taskName() == name {
			alias = shell
		}
	}
	if found == nil {
		found = alias
	}
	if found == nil {
		return shellView{}, nil
	}
	view, _ := found.hostedView("health")
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
	Directory, Name, UID string
	Env                  []string
	Temporary            bool
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
	mux.HandleFunc("POST /shells", func(w http.ResponseWriter, r *http.Request) {
		var req hostedShellRequest
		if !readDaemonBody(w, r, &req) {
			return
		}
		if !validTaskUID(req.UID) || validSession(req.Name) != nil || !filepath.IsAbs(req.Directory) {
			http.Error(w, "invalid shell creation", 400)
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
	req := hostedShellRequest{Directory: cwd, Name: id.name, UID: id.uid, Env: env, Temporary: temporary}
	return daemonJSON(ctx, record, "/shells", req, nil)
}

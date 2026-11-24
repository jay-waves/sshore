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

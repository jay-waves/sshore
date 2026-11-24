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
	"slices"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
)

func hostedDiagnostics(views []shellView, groups []int, detail string) {
	if detail != "health" {
		for i := range views {
			views[i].Directory = sessionDirectory(views[i].PID)
		}
	}
	if len(views) != 0 && (detail == "" || detail == "sessions") {
		snapshot := newProcessSnapshot()
		for i, group := range groups {
			views[i].Foreground = snapshot.foregroundPrograms(group)
			if detail == "sessions" {
				views[i].ProcessTree = snapshot.sessionTree(views[i].PID)
			}
		}
	}
	slices.SortFunc(views, func(a, b shellView) int { return strings.Compare(a.Instance, b.Instance) })
}

// One best-effort snapshot supplies names and trees for display. Process
// ownership and identity checks used for signaling are performed separately.
type processSnapshot struct {
	processes map[int]proc.Process
	children  map[int][]int
	groups    map[int][]string
}

func newProcessSnapshot() processSnapshot {
	processes := proc.ReadProcesses()
	snapshot := processSnapshot{
		processes: make(map[int]proc.Process, len(processes)),
		children:  make(map[int][]int),
		groups:    make(map[int][]string),
	}
	for _, process := range processes {
		process.Name = cleanText(process.Name)
		if process.Group > 0 {
			snapshot.groups[process.Group] = append(snapshot.groups[process.Group], process.Name)
		}
		if process.Parent >= 0 {
			snapshot.processes[process.PID] = process
			snapshot.children[process.Parent] = append(snapshot.children[process.Parent], process.PID)
		}
	}
	return snapshot
}

func (snapshot processSnapshot) foregroundPrograms(group int) string {
	names := snapshot.groups[group]
	if len(names) == 0 {
		return "—"
	}
	return strings.Join(names, " | ")
}

// Display same-user descendants in PID order. Limit depth and nodes so a
// pathological tree cannot flood output.
func (snapshot processSnapshot) sessionTree(shellPID int) string {
	foreground := snapshot.processes[shellPID].Foreground
	var out strings.Builder
	seen := map[int]bool{}
	var walk func(int, string, bool, int)
	walk = func(pid int, prefix string, last bool, depth int) {
		branch, next := "├─ ", "│  "
		if last {
			branch, next = "└─ ", "   "
		}
		if depth == 0 {
			branch, next = "", ""
		}
		if seen[pid] {
			return
		}
		if depth >= 16 || len(seen) >= 128 {
			fmt.Fprintf(&out, "%s%s... [tree truncated]\n", prefix, branch)
			return
		}
		seen[pid] = true
		p, ok := snapshot.processes[pid]
		if !ok {
			fmt.Fprintf(&out, "%s%s%d [process unavailable]\n", prefix, branch, pid)
			return
		}
		label := ""
		if foreground > 0 && p.Group == foreground {
			label = " [fg]"
		}
		fmt.Fprintf(&out, "%s%s%s (%d)%s\n", prefix, branch, p.Name, pid, label)
		for i, child := range snapshot.children[pid] {
			walk(child, prefix+next, i == len(snapshot.children[pid])-1, depth+1)
			if len(seen) >= 128 {
				break
			}
		}
	}
	walk(shellPID, "", true, 0)
	return out.String()
}

func currentShellDirectory(pid int, fallback string) string {
	if pid > 0 {
		if cwd, err := proc.WorkingDirectory(pid); err == nil {
			return cwd
		}
	}
	return fallback
}

func sessionDirectory(pid int) string {
	home, _ := os.UserHomeDir()
	return shortDirectory(currentShellDirectory(pid, ""), home)
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

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
	"golang.org/x/sys/unix"
)

func sessionTasks(roots []setupProcess) ([]proc.Task, error) {
	all, err := proc.ReadTasks()
	if err != nil {
		return nil, err
	}
	selected := make(map[int]bool)
	sessions := make(map[int]bool)
	for _, root := range roots {
		if task, ok := all[root.PID]; ok {
			if task.Start != root.Start {
				return nil, fmt.Errorf("shell PID changed; review setup again")
			}
			selected[root.PID] = true
			if task.Session == task.PID {
				sessions[task.Session] = true
			}
			// PTY children lead their own session. Include detached process groups still
			// belonging to that shell session, not unrelated processes in the account.
			for _, child := range all {
				if child.Parent == root.PID && child.Session == child.PID {
					sessions[child.Session] = true
				}
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for pid, task := range all {
			if !selected[pid] && (selected[task.Parent] || sessions[task.Session]) {
				selected[pid] = true
				changed = true
			}
		}
	}
	var tasks []proc.Task
	for pid := range selected {
		tasks = append(tasks, all[pid])
	}
	return tasks, nil
}

// Cancellation kills only confirmed same-user processes belonging to these roots.
func stopOwnedProcesses(roots []setupProcess) error {
	for _, root := range roots {
		if root.Version != "" {
			v, err := executableVersion(proc.ExecutablePath(root.PID))
			if err != nil || v != root.Version {
				return fmt.Errorf("shell %d changed before cleanup; review again", root.PID)
			}
		}
	}
	tasks, err := sessionTasks(roots)
	if err != nil {
		return err
	}
	// Verify signaling support/ownership for every task before changing anything.
	for _, task := range tasks {
		if err := proc.Signal(task.PID, task.Start, 0); err != nil {
			return err
		}
	}
	for _, task := range tasks {
		if err := proc.Signal(task.PID, task.Start, unix.SIGKILL); err != nil {
			return err
		}
	}
	if waitOwnedProcesses(tasks, 2*time.Second) {
		return nil
	}
	var remaining []string
	for _, task := range tasks {
		if current, err := proc.ReadTask(task.PID); err == nil && current.Start == task.Start && current.State != "Z" && current.State != "X" {
			remaining = append(remaining, strconv.Itoa(task.PID))
		}
	}
	return fmt.Errorf("tasks did not exit after KILL (PIDs %s)", strings.Join(remaining, ", "))
}

func waitOwnedProcesses(tasks []proc.Task, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive := false
		for _, task := range tasks {
			if current, err := proc.ReadTask(task.PID); err == nil && current.Start == task.Start && current.State != "Z" && current.State != "X" {
				alive = true
				break
			}
		}
		if !alive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// One best-effort snapshot supplies names and trees for display. Process
// ownership and identity checks used for signaling are performed separately.
type processSnapshot struct {
	processes map[int]proc.Process
	children  map[int][]int
	groups    map[int][]string
}

func newProcessSnapshot(processes []proc.Process) processSnapshot {
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

// Prefer the foreground group leader's cwd, falling back to the session shell.
// This is a best-effort display snapshot, never an authorization decision.
func sessionDirectory(group, shell int) string {
	for _, pid := range []int{group, shell} {
		if pid <= 0 {
			continue
		}
		if cwd, err := proc.WorkingDirectory(pid); err == nil {
			home, _ := os.UserHomeDir()
			return shortDirectory(cwd, home)
		}
	}
	return "—"
}

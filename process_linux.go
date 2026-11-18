package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type setupTask struct {
	pid, parent, session int
	start, state         string
}

func readSetupTask(pid int) (setupTask, error) {
	var task setupTask
	path := fmt.Sprintf("/proc/%d", pid)
	info, err := os.Stat(path)
	if err != nil {
		return task, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return task, fmt.Errorf("process %d is not owned by this user", pid)
	}
	data, err := os.ReadFile(filepath.Join(path, "stat"))
	if err != nil {
		return task, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return task, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return task, fmt.Errorf("short process stat")
	}
	parent, e1 := strconv.Atoi(fields[1])
	session, e2 := strconv.Atoi(fields[3])
	if e1 != nil || e2 != nil {
		return task, fmt.Errorf("invalid process ancestry")
	}
	return setupTask{pid: pid, parent: parent, session: session, start: fields[19], state: fields[0]}, nil
}
func processStart(pid int) (string, error) { task, err := readSetupTask(pid); return task.start, err }

func sessionTasks(roots []setupProcess) ([]setupTask, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	all := make(map[int]setupTask)
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil {
			continue
		}
		if task, e := readSetupTask(pid); e == nil {
			all[pid] = task
		}
	}
	selected := make(map[int]bool)
	sessions := make(map[int]bool)
	for _, root := range roots {
		if task, ok := all[root.PID]; ok {
			if task.start != root.Start {
				return nil, fmt.Errorf("shell PID changed; review setup again")
			}
			selected[root.PID] = true
			if task.session == task.pid {
				sessions[task.session] = true
			}
			// PTY children lead their own session. Include detached process groups still
			// belonging to that shell session, not unrelated processes in the account.
			for _, child := range all {
				if child.parent == root.PID && child.session == child.pid {
					sessions[child.session] = true
				}
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for pid, task := range all {
			if !selected[pid] && (selected[task.parent] || sessions[task.session]) {
				selected[pid] = true
				changed = true
			}
		}
	}
	var tasks []setupTask
	for pid := range selected {
		tasks = append(tasks, all[pid])
	}
	return tasks, nil
}

// Cancellation kills only confirmed same-user processes belonging to these roots.
func stopOwnedProcesses(roots []setupProcess) error {
	for _, root := range roots {
		if root.Version != "" {
			v, err := executableVersion(fmt.Sprintf("/proc/%d/exe", root.PID))
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
		if err := signalProcess(task.pid, task.start, 0); err != nil {
			return err
		}
	}
	for _, task := range tasks {
		if err := signalProcess(task.pid, task.start, unix.SIGKILL); err != nil {
			return err
		}
	}
	if waitOwnedProcesses(tasks, 2*time.Second) {
		return nil
	}
	var remaining []string
	for _, task := range tasks {
		if current, err := readSetupTask(task.pid); err == nil && current.start == task.start && current.state != "Z" && current.state != "X" {
			remaining = append(remaining, strconv.Itoa(task.pid))
		}
	}
	return fmt.Errorf("tasks did not exit after KILL (PIDs %s)", strings.Join(remaining, ", "))
}

func waitOwnedProcesses(tasks []setupTask, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive := false
		for _, task := range tasks {
			if current, err := readSetupTask(task.pid); err == nil && current.start == task.start && current.state != "Z" && current.state != "X" {
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

// Session listings show a best-effort /proc snapshot rooted at the shell PID.
// Only same-user descendants are included, in PID order. Display comm names,
// never command lines or environment, and sanitize terminal control characters.
// Tree branches distinguish ancestry; [fg] marks the shell PTY's active
// process group. Limit depth/nodes so pathological trees cannot flood output.

type listedProcess struct {
	pid, parent, group, foreground int
	name                           string
}

func sessionProcessTree(root string, shellPID int) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Sprintf("%d [process unavailable]\n", shellPID)
	}
	processes := map[int]listedProcess{}
	children := map[int][]int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Geteuid()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(path, "stat"))
		if err != nil {
			continue
		}
		stat := string(data)
		start, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
		if start < 0 || end <= start {
			continue
		}
		fields := strings.Fields(stat[end+1:])
		if len(fields) < 6 {
			continue
		}
		parent, e1 := strconv.Atoi(fields[1])
		group, e2 := strconv.Atoi(fields[2])
		foreground, e3 := strconv.Atoi(fields[5])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		processes[pid] = listedProcess{pid, parent, group, foreground, cleanText(stat[start+1 : end])}
		children[parent] = append(children[parent], pid)
	}
	for parent := range children {
		sort.Ints(children[parent])
	}
	foreground := processes[shellPID].foreground
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
		p, ok := processes[pid]
		if !ok {
			fmt.Fprintf(&out, "%s%s%d [process unavailable]\n", prefix, branch, pid)
			return
		}
		label := ""
		if foreground > 0 && p.group == foreground {
			label = " [fg]"
		}
		fmt.Fprintf(&out, "%s%s%s (%d)%s\n", prefix, branch, p.name, pid, label)
		for i, child := range children[pid] {
			walk(child, prefix+next, i == len(children[pid])-1, depth+1)
			if len(seen) >= 128 {
				break
			}
		}
	}
	walk(shellPID, "", true, 0)
	return out.String()
}

// Snapshot the PTY foreground process group only when listing sessions. proc stat
// contains comm in parentheses (possibly with spaces or parentheses itself), so
// split at the last closing parenthesis before parsing pgrp. Processes can exit
// during the scan; unreadable entries are skipped. Names are shown in PID order,
// joined with " | ": this groups foreground processes, not reconstructed shell
// pipeline syntax. Never expose command-line arguments or background jobs.

func foregroundStat(stat string) (string, int, bool) {
	start, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if start < 0 || end <= start {
		return "", 0, false
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 3 {
		return "", 0, false
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil || group <= 0 {
		return "", 0, false
	}
	return stat[start+1 : end], group, true
}

func foregroundPrograms(root string, group int) string {
	if group <= 0 {
		return "—"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "—"
	}
	type process struct {
		pid  int
		name string
	}
	var processes []process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		info, err := os.Stat(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Geteuid()) {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(root, entry.Name(), "stat"))
		if err != nil {
			continue
		}
		name, pgid, ok := foregroundStat(string(stat))
		if ok && pgid == group {
			processes = append(processes, process{pid, cleanText(name)})
		}
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].pid < processes[j].pid })
	var names []string
	for _, p := range processes {
		names = append(names, p.name)
	}
	if len(names) == 0 {
		return "—"
	}
	return strings.Join(names, " | ")
}

// Prefer the foreground group leader's cwd, falling back to the session shell.
// This is a best-effort display snapshot, never an authorization decision.
func sessionDirectory(group, shell int) string {
	for _, pid := range []int{group, shell} {
		if pid <= 0 {
			continue
		}
		if _, err := readSetupTask(pid); err != nil {
			continue
		}
		if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
			home, _ := os.UserHomeDir()
			return shortDirectory(cwd, home)
		}
	}
	return "—"
}

// pidfds bind signals to the inspected process, never a reused numeric PID.
func signalProcess(pid int, start string, signal unix.Signal) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	current, err := processStart(pid)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != start {
		return fmt.Errorf("process %d changed during setup", pid)
	}
	if err = unix.PidfdSendSignal(fd, signal, nil, 0); errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

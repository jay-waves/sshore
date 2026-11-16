//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

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

package main

// Session listings show a best-effort /proc snapshot rooted at the shell PID.
// Only same-user descendants are included, in PID order. Display comm names,
// never command lines or environment, and sanitize terminal control characters.
// ASCII branches distinguish ancestry; foreground marks the shell PTY's active
// process group. Limit depth/nodes so pathological trees cannot flood output.
import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type listedProcess struct {
	pid, parent, group, foreground int
	name                           string
}

func sessionProcessTree(root string, shellPID int) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Sprintf("`-- %d [process unavailable]\n", shellPID)
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
		branch, next := "|-- ", "|   "
		if last {
			branch, next = "`-- ", "    "
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
			label = " [foreground]"
		}
		fmt.Fprintf(&out, "%s%s%d %s%s\n", prefix, branch, pid, p.name, label)
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

package main

// Snapshot the PTY foreground process group only when listing sessions. proc stat
// contains comm in parentheses (possibly with spaces or parentheses itself), so
// split at the last closing parenthesis before parsing pgrp. Processes can exit
// during the scan; unreadable entries are skipped. Names are shown in PID order,
// joined with " | ": this groups foreground processes, not reconstructed shell
// pipeline syntax. Never expose command-line arguments or background jobs.
import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

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

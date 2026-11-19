package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const procRoot = "/proc"

func processPath(pid int) string { return filepath.Join(procRoot, strconv.Itoa(pid)) }

// ExecutablePath returns the procfs link for opening a process executable.
func ExecutablePath(pid int) string { return filepath.Join(processPath(pid), "exe") }

// WorkingDirectory reads the current directory of a same-user process.
func WorkingDirectory(pid int) (string, error) {
	if _, err := ReadTask(pid); err != nil {
		return "", err
	}
	return os.Readlink(filepath.Join(processPath(pid), "cwd"))
}

func readOwnedStat(path string, pid int) (string, []string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return "", nil, fmt.Errorf("process %d is not owned by this user", pid)
	}
	data, err := os.ReadFile(filepath.Join(path, "stat"))
	if err != nil {
		return "", nil, err
	}
	// comm can contain spaces, newlines, and parentheses.
	stat := string(data)
	start, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if start < 0 || end <= start {
		return "", nil, fmt.Errorf("invalid process stat")
	}
	return stat[start+1 : end], strings.Fields(stat[end+1:]), nil
}

// ReadTasks enumerates same-user task identities. Processes that disappear or
// cannot be read during enumeration are skipped.
func ReadTasks() (map[int]Task, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	tasks := make(map[int]Task)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if task, err := ReadTask(pid); err == nil {
			tasks[pid] = task
		}
	}
	return tasks, nil
}

// Process holds advisory metadata from procfs. Parent and Foreground are -1
// when the corresponding ancestry and terminal fields cannot both be read.
type Process struct {
	PID, Parent, Group, Foreground int
	Name                           string
}

// ReadProcesses returns a best-effort snapshot of same-user processes in PID order.
func ReadProcesses() []Process { return readProcesses(procRoot) }

func readProcesses(root string) []Process {
	entries, _ := os.ReadDir(root)
	var processes []Process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		name, fields, err := readOwnedStat(filepath.Join(root, entry.Name()), pid)
		if err != nil || len(fields) < 3 {
			continue
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		process := Process{PID: pid, Parent: -1, Group: group, Foreground: -1, Name: name}
		if len(fields) >= 6 {
			parent, parentErr := strconv.Atoi(fields[1])
			foreground, foregroundErr := strconv.Atoi(fields[5])
			if parentErr == nil && foregroundErr == nil {
				process.Parent, process.Foreground = parent, foreground
			}
		}
		processes = append(processes, process)
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes
}

package proc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// StopSession stops the identified shell and its same-user session processes.
func StopSession(pid int, start string) error {
	tasks, err := sessionTasks(pid, start)
	if err != nil {
		return err
	}
	// Preflight the entire selection before sending any terminating signal.
	for _, t := range tasks {
		if err := signalTask(t, 0); err != nil {
			return err
		}
	}
	for _, t := range tasks {
		if err := signalTask(t, unix.SIGKILL); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	var remaining []string
	for {
		remaining = remaining[:0]
		for _, t := range tasks {
			if current, err := LiveIdentity(t.pid); err == nil && current == t.start {
				remaining = append(remaining, strconv.Itoa(t.pid))
			}
		}
		if len(remaining) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("tasks did not exit after KILL (PIDs %s)", strings.Join(remaining, ", "))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func sessionTasks(pid int, start string) ([]task, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	all := make(map[int]task)
	for _, entry := range entries {
		id, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if t, err := readTask(id); err == nil {
			all[id] = t
		}
	}
	root, ok := all[pid]
	if !ok {
		return nil, nil
	}
	if root.start != start {
		return nil, fmt.Errorf("shell PID changed before shutdown")
	}
	selected := map[int]bool{pid: true}
	sessions := make(map[int]bool)
	if root.session == pid {
		sessions[pid] = true
	}
	// Include PTY child sessions, including background process groups within them.
	for _, child := range all {
		if child.parent == pid && child.session == child.pid {
			sessions[child.session] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for id, t := range all {
			if !selected[id] && (selected[t.parent] || sessions[t.session]) {
				selected[id] = true
				changed = true
			}
		}
	}
	tasks := make([]task, 0, len(selected))
	for id := range selected {
		tasks = append(tasks, all[id])
	}
	return tasks, nil
}

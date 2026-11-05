package main

import (
	"fmt"
	"os"
	"path/filepath"
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
				return nil, fmt.Errorf("session PID changed; review setup again")
			}
			selected[root.PID] = true
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

func stopOldSessions(roots []setupProcess) error {
	for _, root := range roots {
		if root.Version != "" {
			v, err := executableVersion(fmt.Sprintf("/proc/%d/exe", root.PID))
			if err != nil || v != root.Version {
				return fmt.Errorf("session %d changed before cleanup; review again", root.PID)
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
		if err := signalProcess(task.pid, task.start, unix.SIGTERM); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, task := range tasks {
			if now, e := readSetupTask(task.pid); e == nil && now.start == task.start && now.state != "Z" {
				alive = true
				break
			}
		}
		if !alive {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	for _, task := range tasks {
		if err := signalProcess(task.pid, task.start, unix.SIGKILL); err != nil {
			return err
		}
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, task := range tasks {
			if now, e := readSetupTask(task.pid); e == nil && now.start == task.start && now.state != "Z" {
				alive = true
				break
			}
		}
		if !alive {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("some old session tasks did not exit; complete cleanup manually")
}

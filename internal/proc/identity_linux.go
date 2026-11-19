// Package proc provides operating system process identity and waiting.
package proc

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type Task struct {
	PID, Parent, Session int
	Start, State         string
}

func ReadTask(pid int) (Task, error) {
	_, fields, err := readOwnedStat(processPath(pid), pid)
	if err != nil {
		return Task{}, err
	}
	if len(fields) < 20 {
		return Task{}, fmt.Errorf("short process stat")
	}
	parent, e1 := strconv.Atoi(fields[1])
	session, e2 := strconv.Atoi(fields[3])
	if e1 != nil || e2 != nil {
		return Task{}, fmt.Errorf("invalid process ancestry")
	}
	return Task{PID: pid, Parent: parent, Session: session, Start: fields[19], State: fields[0]}, nil
}
func StartToken(pid int) (string, error) { task, err := ReadTask(pid); return task.Start, err }

func Signal(pid int, start string, signal unix.Signal) error {
	// pidfds bind signals to the inspected process, even after PID reuse.
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	current, err := StartToken(pid)
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

func LiveIdentity(pid int) (string, error) {
	p, err := ReadTask(pid)
	if err != nil {
		return "", err
	}
	if p.State == "Z" || p.State == "X" {
		return "", fmt.Errorf("parent shell exited")
	}
	return p.Start, nil
}

func processName(pid int) (string, error) {
	data, err := os.ReadFile(processPath(pid) + "/comm")
	return strings.TrimSpace(string(data)), err
}

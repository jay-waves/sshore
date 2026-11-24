// Package proc reads process identities and diagnostics and stops Linux sessions.
package proc

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

type task struct {
	pid, parent, session int
	start, state         string
}

func readTask(pid int) (task, error) {
	_, fields, err := readOwnedStat(processPath(pid), pid)
	if err != nil {
		return task{}, err
	}
	if len(fields) < 20 {
		return task{}, fmt.Errorf("short process stat")
	}
	parent, e1 := strconv.Atoi(fields[1])
	session, e2 := strconv.Atoi(fields[3])
	if e1 != nil || e2 != nil {
		return task{}, fmt.Errorf("invalid process ancestry")
	}
	return task{pid: pid, parent: parent, session: session, start: fields[19], state: fields[0]}, nil
}

func signalTask(t task, signal unix.Signal) error {
	// pidfds bind signals to the inspected process, even after PID reuse.
	fd, err := unix.PidfdOpen(t.pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	current, err := readTask(t.pid)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.start != t.start {
		return fmt.Errorf("process %d changed before signaling", t.pid)
	}
	if err = unix.PidfdSendSignal(fd, signal, nil, 0); errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func LiveIdentity(pid int) (string, error) {
	p, err := readTask(pid)
	if err != nil {
		return "", err
	}
	if p.state == "Z" || p.state == "X" {
		return "", fmt.Errorf("parent shell exited")
	}
	return p.start, nil
}

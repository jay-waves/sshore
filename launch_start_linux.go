//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"
)

// Selection opens no task streams. Only daemon registration claims a new identity.
type shellSelection struct {
	taskIdentitySelection
	existing bool
}

func selectHostedShell(name, mode string) (shellSelection, error) {
	attach := mode == "attach"
	if name != "" {
		task, err := resolveHostedShell(name, "")
		if err != nil {
			return shellSelection{}, err
		}
		if task.Instance != "" {
			if mode == "new" || mode == "spawn" {
				return shellSelection{}, fmt.Errorf("shell %s already exists; use sshore attach HOST:%s", name, name)
			}
			return shellSelection{taskIdentitySelection: taskIdentitySelection{name: task.Name, uid: task.Instance}, existing: true}, nil
		}
	}
	if attach {
		if name == "" {
			return shellSelection{}, fmt.Errorf("attach requires a shell name")
		}
		return shellSelection{}, fmt.Errorf("shell %s does not exist; use sshore list HOST", name)
	}
	id, err := chooseTaskIdentity("shell", name)
	if err != nil {
		return shellSelection{}, err
	}
	return shellSelection{taskIdentitySelection: *id}, nil
}
func prepareHostedShell(name, mode, executable string, gate *os.File) error {
	defer gate.Close()
	if (mode == "new" || mode == "spawn") && isShortTaskID(name) {
		name = ""
	}
	selected, err := selectHostedShell(name, mode)
	if err != nil {
		return err
	}
	if !selected.existing {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		if err := createRemoteShell(executable, selected.taskIdentitySelection, mode == "spawn", cwd, os.Environ()); err != nil {
			return err
		}
	}
	record, err := existingDaemonRecord()
	if err != nil {
		return err
	}
	record.UID = selected.uid
	if err := checkPeerVersion(record.Version, "daemon"); err != nil {
		return err
	}
	gate.Close()
	return writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: record.UID, LocalRequest: &localDaemonRequest{Record: record}})
}

// Keep each label with the instance it represents until selection is complete.
func detachedHostedShells() ([]sessionTarget, error) {
	rows, err := hostedShells("health")
	if err != nil {
		return nil, err
	}
	var targets []sessionTarget
	for _, shell := range rows {
		if shell.State == "detached" {
			targets = append(targets, sessionTarget{Name: shell.Name, Instance: shell.Instance})
		}
	}
	if len(targets) > 128 {
		return nil, fmt.Errorf("too many shells to select; use sshore list HOST, then sshore attach HOST:ID")
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets, nil
}

func detachHelper() error {
	sock := os.Getenv("SSHORE_SOCKET")
	if sock == "" {
		return fmt.Errorf("run this command inside an sshore shell")
	}
	c, err := dialHostedTask(context.Background(), sock, requestTimeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(requestTimeout))
	if err := writeMessage(c, message{Type: "detach"}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if m.Type != "detached" {
		return fileResponseError(m)
	}
	return nil
}

func (s *remoteShell) sessionTargets(request message) message {
	s.mu.Lock()
	current := s.record.Name
	s.mu.Unlock()
	reply := message{Type: "session_targets", Action: request.Action, Name: current}
	rows, err := detachedHostedShells()
	if err != nil {
		reply.Error = err.Error()
		return reply
	}
	for _, shell := range rows {
		if request.Action != "select" || request.Name == shell.Name {
			reply.Targets = append(reply.Targets, shell)
		}
	}
	if request.Action == "select" && len(reply.Targets) != 1 {
		reply.Error = fmt.Sprintf("shell %s is unavailable", cleanText(request.Name))
	}
	return reply
}

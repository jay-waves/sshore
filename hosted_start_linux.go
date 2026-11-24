//go:build linux

package main

import (
	"context"
	"errors"
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

func selectHostedShell(name, mode string, expected ...string) (shellSelection, error) {
	if mode == "attach" {
		if name == "" {
			return shellSelection{}, fmt.Errorf("attach requires a shell name")
		}
		task, err := resolveHostedShell(name, firstExpectedInstance(expected))
		if err != nil {
			return shellSelection{}, err
		}
		if task.Instance == "" {
			return shellSelection{}, fmt.Errorf("shell %s does not exist; check known IDs with sshore list", name)
		}
		return shellSelection{taskIdentitySelection: taskIdentitySelection{name: task.Name, uid: task.Instance}, existing: true}, nil
	}
	id, err := chooseTaskIdentity("shell", name)
	if err != nil {
		if errors.Is(err, errObjectNameInUse) {
			return shellSelection{}, fmt.Errorf("name %s already exists; use a different shell name", name)
		}
		return shellSelection{}, err
	}
	return shellSelection{taskIdentitySelection: *id}, nil
}
func prepareHostedShell(name, mode, instance, executable string, gate *os.File) error {
	defer gate.Close()
	if (mode == "new") && isShortTaskID(name) {
		name = ""
	}
	selected, err := selectHostedShell(name, mode, instance)
	if err != nil {
		return err
	}
	if !selected.existing {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		if err := createRemoteShell(executable, selected.taskIdentitySelection, cwd, os.Environ()); err != nil {
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
func detachedHostedShells(parent string) ([]sessionTarget, error) {
	rows, err := hostedShells("health")
	if err != nil {
		return nil, err
	}
	var targets []sessionTarget
	for _, shell := range rows {
		if shell.State == taskDetached && (shell.Owner == "" || shell.Owner == parent) {
			targets = append(targets, sessionTarget{Name: shell.Name, Instance: shell.Instance})
		}
	}
	if len(targets) > 128 {
		return nil, fmt.Errorf("too many shells to select; use sshore attach HOST:ID")
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets, nil
}

func retentionHostedShell(m message) error {
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
	return requestShellRetention(c, m)
}

func (s *remoteShell) sessionTargets(request message) message {
	s.mu.Lock()
	current := s.record.Name
	s.mu.Unlock()
	reply := message{Type: "session_targets", Action: request.Action, Name: current}
	rows, err := detachedHostedShells(s.treeIdentity())
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

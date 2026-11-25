//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jay-waves/sshore/internal/sys"
)

func prepareHostedShell(name, mode, instance, executable string, gate *sys.FileLock, openIfUnavailable bool) error {
	defer gate.Close()
	if mode == "new" && isShortTaskID(name) {
		name = ""
	}
	var record localShellRecord
	var err error
	var reason string
	if mode == "attach" {
		var target selectedTask
		if openIfUnavailable {
			target, reason, err = selectRemoteAttachShell(name, instance)
		} else {
			target, err = resolveHostedShell(name, instance)
		}
		if err != nil {
			return err
		}
		if reason != "" {
			name, mode = "", "new"
		} else if target.Instance == "" {
			return fmt.Errorf("shell %s does not exist; check known IDs with sshore list", name)
		} else {
			record, err = existingDaemonRecord()
			record.UID = target.Instance
		}
	}
	if mode != "attach" {
		var cwd string
		cwd, err = os.Getwd()
		if err == nil {
			record, err = createHostedShell(context.Background(), executable, name, cwd, os.Environ())
		}
	}
	if err != nil {
		return err
	}
	if err := checkPeerVersion(record.Version, "daemon"); err != nil {
		return err
	}
	gate.Close()
	return writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", Instance: record.UID, Reason: reason, LocalRequest: &localDaemonRequest{Record: record}})
}

func selectRemoteAttachShell(ref, instance string) (selectedTask, string, error) {
	query := taskQuery{Kind: "hosted", Ref: ref, UID: instance}
	if !validTaskUID(instance) {
		rows, err := queryRegisteredTasks(taskQuery{Kind: "hosted"})
		if err != nil {
			return selectedTask{}, "", err
		}
		target, err := matchTask(ref, rows)
		if err != nil {
			return selectedTask{}, err.Error(), nil
		}
		if target.Instance == "" {
			return selectedTask{}, fmt.Sprintf("Shell %s was not found", ref), nil
		}
		query.UID = target.Instance
	}
	rows, err := queryRegisteredTasks(query)
	if err != nil {
		return selectedTask{}, "", err
	}
	if len(rows) == 0 {
		return selectedTask{}, fmt.Sprintf("Shell %s was not found", ref), nil
	}
	query.Detached = true
	idle, err := queryRegisteredTasks(query)
	if err != nil {
		return selectedTask{}, "", err
	}
	if len(idle) == 0 {
		return selectedTask{}, fmt.Sprintf("Shell %s is in use or has a parent shell", ref), nil
	}
	return idle[0], "", nil
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

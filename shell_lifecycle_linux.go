package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Public names resolve through the account daemon's task table. Control and
// data connections select the immutable ID at the authenticated TCP endpoint.

func resolveSession(name string, expected ...string) (string, error) {
	task, err := resolveHostedShell(name, firstExpectedInstance(expected))
	return task.Instance, err
}

func resolveHostedShell(name, instance string) (selectedTask, error) {
	if err := validSession(name); err != nil {
		return selectedTask{}, err
	}
	rows, err := queryRegisteredTasks(taskQuery{Kind: "hosted", Ref: name, UID: instance})
	if err != nil || len(rows) == 0 {
		return selectedTask{}, err
	}
	return rows[0], nil
}

func renameSession(old, name string, expected ...string) error {
	if err := validAlias(name); err != nil {
		return err
	}
	task, err := resolveHostedShell(old, firstExpectedInstance(expected))
	if err != nil {
		return err
	}
	if task.Instance == "" {
		return fmt.Errorf("shell %s does not exist", old)
	}

	// The daemon rechecks the selected label and UID when it changes the name.
	c, err := dialHostedTask(context.Background(), task.Instance, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, message{Type: "rename", Instance: task.Instance, Name: name, Action: task.Name, SoftwareVersion: version}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if m.Type != "renamed" {
		return fmt.Errorf("rename: %s", m.Error)
	}
	fmt.Fprintf(os.Stdout, "Renamed shell %s to %s\n", task.Name, name)
	return nil
}

func firstExpectedInstance(expected []string) string {
	if len(expected) > 0 {
		return expected[0]
	}
	return ""
}

// Cancellation is authorized and applied by the task owner. Process identity
// validation belongs to the daemon immediately before it signals its child.
func killSession(name string, expected ...string) error {
	id, err := resolveSession(name, expected...)
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("shell %s does not exist or its instance changed", name)
	}
	c, err := dialHostedTask(context.Background(), id, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, message{Type: "kill", Instance: id, SoftwareVersion: version}); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return err
	}
	if reply.Type != "killed" {
		return fmt.Errorf("kill shell: %s", reply.Error)
	}
	fmt.Fprintf(os.Stdout, "Ended shell %s\n", name)
	return nil
}

// Older releases wrote receipts beside their former sockets. Read those
// observations until they expire and allow reset to clear their known files.
func legacyShellRuntime() string {
	if dir := os.Getenv("SSHORE_RUNTIME_DIR"); dir != "" {
		return dir
	}
	if root := os.Getenv("XDG_RUNTIME_DIR"); root != "" {
		return filepath.Join(root, "sshore")
	}
	return fmt.Sprintf("/tmp/sshore-%d", os.Geteuid())
}

func resetHostedExitState(account string, directories []string) error {
	// Reset holds the account owner lock, so no daemon can write new receipts.
	directories = append(directories, account)
	// A stale runtime override cannot block reset of current state.
	if dir := legacyShellRuntime(); filepath.IsAbs(dir) && privateStatePath(dir, true) == nil {
		directories = append(directories, dir)
	}
	for _, dir := range directories {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("invalid legacy runtime directory")
		}
		if err := privateStatePath(dir, true); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			id, receipt := strings.CutSuffix(name, ".exit.json")
			socketID, socket := strings.CutSuffix(name, ".sock")
			if receipt && validTaskUID(strings.TrimSuffix(id, ".sock")) || socket && validTaskUID(socketID) || name == ".exit.lock" {
				if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
	}
	return nil
}

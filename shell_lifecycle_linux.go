package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Public names resolve through the account daemon's task table. Control and
// data connections select the immutable ID at the authenticated TCP endpoint.

func resolveSession(name string, expected ...string) (string, error) {
	task, err := resolveHostedShell(name, firstExpectedInstance(expected))
	return task.Instance, err
}

func resolveHostedShell(name, instance string) (shellView, error) {
	if err := validSession(name); err != nil {
		return shellView{}, err
	}
	if active := accountActivity.Load(); active != nil && active.tasks != nil {
		return active.tasks.resolveHostedShell(name, instance)
	}
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return shellView{}, nil
	}
	if err != nil {
		return shellView{}, err
	}
	query := url.Values{"name": {name}}
	if instance != "" {
		query.Set("instance", instance)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var view shellView
	err = daemonJSON(ctx, record, "/tasks/hosted/resolve?"+query.Encode(), nil, &view)
	return view, err
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

// The owner lock gives a single daemon write access. Its shell goroutines
// serialize writes and pruning so the receipt limit applies across all shells.
var shellExitMu sync.Mutex

// Exit receipts are best-effort diagnostics. Keep at most 100 receipts for
// seven days. Atomic writes and bounded no-follow reads protect the records.
type sessionExit struct {
	Name   string    `json:"name"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	file   string
}

const sessionExitMaxBytes = 4096

func readSessionExits(dir string) []sessionExit {
	if privateStatePath(dir, true) != nil {
		return nil
	}
	entries, _ := os.ReadDir(dir)
	var records []sessionExit
	for _, e := range entries {
		id, receipt := strings.CutSuffix(e.Name(), ".exit.json")
		if !receipt || !validTaskUID(strings.TrimSuffix(id, ".sock")) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		f := os.NewFile(uintptr(fd), path)
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > sessionExitMaxBytes {
			f.Close()
			continue
		}
		var record sessionExit
		err = readBoundedStateJSON(f, sessionExitMaxBytes, &record)
		f.Close()
		if err != nil || validSession(record.Name) != nil || record.At.IsZero() {
			continue
		}
		record.file = path
		records = append(records, record)
	}
	return records
}

func saveHostedExit(id, name, reason string) {
	if !validTaskUID(id) || validSession(name) != nil || reason == "" {
		return
	}
	dir, err := daemonLocation()
	if err != nil {
		return
	}
	shellExitMu.Lock()
	defer shellExitMu.Unlock()
	path := filepath.Join(dir, id+".exit.json")
	if writeBoundedStateJSON(path, sessionExit{Name: name, Reason: reason, At: time.Now().UTC()}, sessionExitMaxBytes) != nil {
		return
	}
	_, discarded := partitionSessionExits(readSessionExits(dir))
	for _, record := range discarded {
		_ = os.Remove(record.file)
	}
}

func partitionSessionExits(records []sessionExit) ([]sessionExit, []sessionExit) {
	cutoff := time.Now().Add(-exitReceiptLifetime)
	return partitionExitReceipts(records, cutoff, func(record sessionExit) time.Time { return record.At })
}

func hostedExitHistory() []exitView {
	exits := []exitView{}
	account, _ := daemonLocation()
	records := readSessionExits(account)
	if dir := legacyShellRuntime(); filepath.IsAbs(dir) && dir != account {
		records = append(records, readSessionExits(dir)...)
	}
	recent, _ := partitionSessionExits(records)
	for _, r := range recent {
		exits = append(exits, exitView{Name: r.Name, Reason: r.Reason, Time: r.At})
	}
	return exits
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

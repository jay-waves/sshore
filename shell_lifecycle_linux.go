package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

func (s *remoteShell) rename(old, name string, expected ...string) error {
	return s.renameTask(s.tasks, s.instance, name, func() (string, error) {
		if instance := firstExpectedInstance(expected); instance != "" && instance != s.instance {
			return "", fmt.Errorf("shell instance changed; refresh with sshore list")
		}
		if s.killing || s.exited || s.name != old {
			return "", fmt.Errorf("shell changed or is exiting; refresh the shell list")
		}
		if s.name == name {
			return "", nil
		}
		s.name = name
		return remoteTreeIdentity(s.instance), nil
	})
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
	path, err := resolveSession(name, expected...)
	if err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("shell %s does not exist or its instance changed", name)
	}
	c, err := dialHostedTask(context.Background(), path, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, message{Type: "kill", Instance: path, SoftwareVersion: version}); err != nil {
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

// Exit receipts are best-effort diagnostics, not evidence of task cleanup. Each
// immutable socket identity has at most one receipt; newer lifetimes replace it.
// Keep at most 100 receipts for seven days in the private runtime directory.
// Atomic 0600 writes and bounded no-follow reads avoid partial/unbounded records.
// Abrupt SIGKILL, reboot or directory removal may leave no receipt. Explicit kill
// and verified update cleanup write receipts after successful process teardown,
// including old daemons that cannot report their own reasons.

type sessionExit struct {
	Name   string    `json:"name"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	file   string
}

const sessionExitMaxBytes = 4096

func readSessionExits(dir string) []sessionExit {
	entries, _ := os.ReadDir(dir)
	var records []sessionExit
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".exit.json") {
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

func saveSessionExit(socket, name, reason string) {
	if validSession(name) != nil || reason == "" {
		return
	}
	dir := filepath.Dir(socket)
	gate, err := openLock(filepath.Join(dir, ".exit.lock"), false)
	if err != nil {
		return
	}
	defer gate.Close()
	if writeBoundedStateJSON(socket+".exit.json", sessionExit{Name: name, Reason: reason, At: time.Now().UTC()}, sessionExitMaxBytes) != nil {
		return
	}
	recentSessionExits(dir, true)
}

// Write-side callers hold the exit lock through pruning.
func recentSessionExits(dir string, prune bool) []sessionExit {
	records := readSessionExits(dir)
	cutoff := time.Now().Add(-exitReceiptLifetime)
	recent, discarded := partitionExitReceipts(records, cutoff, func(record sessionExit) time.Time { return record.At })
	if prune {
		for _, record := range discarded {
			_ = os.Remove(record.file)
		}
	}
	return recent
}

func printSessionExits(dir string, out io.Writer) error {
	gate, err := openLock(filepath.Join(dir, ".exit.lock"), false)
	if err != nil {
		return err
	}
	records := recentSessionExits(dir, true)
	gate.Close()
	if len(records) != 0 {
		fmt.Fprintln(out, "\nRecent exits:")
	}
	for _, record := range records {
		fmt.Fprintf(out, "%s [ended] · %s · %s\n", record.Name, record.At.Local().Format("2006-01-02 15:04:05 MST"), cleanText(record.Reason))
	}
	return nil
}

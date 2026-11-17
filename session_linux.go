package main

import (
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Public names resolve through the account daemon's task table. Control and
// data connections select the immutable ID at the authenticated TCP endpoint.

func resolveSession(name string, expected ...string) (string, error) {
	if err := validSession(name); err != nil {
		return "", err
	}
	records, err := hostedTaskRecords()
	if err != nil {
		return "", err
	}
	instance := firstExpectedInstance(expected)
	var found string
	for _, task := range records {
		match := task.Name == name || task.ID == name || isShortTaskID(name) && strings.HasPrefix(task.ID, strings.ToLower(name))
		if instance != "" {
			match = task.ID == instance
		}
		if match {
			if found != "" {
				return "", fmt.Errorf("ambiguous shell name %s", name)
			}
			found = task.ID
		}
	}
	return found, nil
}

func renameSession(old, name string, expected ...string) error {
	if err := validAlias(name); err != nil {
		return err
	}
	gate, err := setupLock()
	if err != nil {
		return err
	}
	defer gate.Close()
	path, err := resolveSession(old, expected...)
	if err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("shell %s does not exist", old)
	}

	old, err = readSessionName(path)
	if err != nil {
		return err
	}
	gate.Close() // The daemon takes the gate and rechecks the old label and destination.
	c, err := dialHostedTask(context.Background(), path, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, message{Type: "rename", Instance: firstExpectedInstance(expected), Name: name, Action: old, SoftwareVersion: version}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if m.Type != "renamed" {
		return fmt.Errorf("rename: %s", m.Error)
	}
	fmt.Fprintf(os.Stdout, "Renamed shell %s to %s\n", old, name)
	return nil
}

func sessionLabel(m message, filename string) string {
	if validSession(m.Name) == nil {
		return m.Name
	}
	return cleanText(strings.TrimSuffix(filename, ".sock"))
}

func (s *remoteShell) rename(old, name string, expected ...string) error {
	if err := validAlias(name); err != nil {
		return err
	}
	s.tasks.mu.Lock()
	defer s.tasks.mu.Unlock()
	for id, task := range s.tasks.records {
		if id != s.instance && task.taskName() == name {
			return fmt.Errorf("%s: %w", name, errObjectNameInUse)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if firstExpectedInstance(expected) != "" && firstExpectedInstance(expected) != s.instance {
		return fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	if s.lifecycle.Exited() || s.name != old {
		return fmt.Errorf("shell changed or is exiting; refresh the shell list")
	}
	if s.name == name {
		return nil
	}
	s.name = name
	renameShellTree(remoteTreeIdentity(s.instance), name)
	return nil
}

func readSessionName(id string) (string, error) {
	records, err := hostedTaskRecords()
	if err != nil {
		return "", err
	}
	for _, task := range records {
		if task.ID == id {
			return task.Name, nil
		}
	}
	return "", os.ErrNotExist
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
	if err := writeMessage(c, message{Type: "status", Action: "health"}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil || m.Type != "session" {
		return fmt.Errorf("shell status unavailable: %v", err)
	}
	c.Close()
	c, err = dialHostedTask(context.Background(), m.Instance, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, message{Type: "cancel", Instance: m.Instance, SoftwareVersion: version}); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return err
	}
	if reply.Type != "cancelled" {
		return fmt.Errorf("cancel shell: %s", reply.Error)
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
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
			f.Close()
			continue
		}
		var record sessionExit
		err = json.NewDecoder(io.LimitReader(f, 4096)).Decode(&record)
		f.Close()
		if err != nil || validSession(record.Name) != nil || record.At.IsZero() {
			continue
		}
		record.file = path
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].At.After(records[j].At) })
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
	if writeStateJSON(socket+".exit.json", sessionExit{Name: name, Reason: reason, At: time.Now().UTC()}) != nil {
		return
	}
	pruneSessionExits(dir)
}

func pruneSessionExits(dir string) {
	for i, record := range readSessionExits(dir) {
		if i >= 100 || time.Since(record.At) > 7*24*time.Hour {
			_ = os.Remove(record.file)
		}
	}
}

func printSessionExits(dir string, out io.Writer) error {
	gate, err := openLock(filepath.Join(dir, ".exit.lock"), false)
	if err != nil {
		return err
	}
	pruneSessionExits(dir)
	records := readSessionExits(dir)
	gate.Close()
	heading := false
	for _, record := range records {
		if time.Since(record.At) > 7*24*time.Hour {
			continue
		}
		if !heading {
			fmt.Fprintln(out, "\nRecent exits:")
			heading = true
		}
		fmt.Fprintf(out, "%s [ended] · %s · %s\n", record.Name, record.At.Local().Format("2006-01-02 15:04:05 MST"), cleanText(record.Reason))
	}
	return nil
}

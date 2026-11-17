package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Public names are mutable labels on immutable daemon/socket identities. Resolve
// labels under the account setup lock before creating, attaching, renaming or
// killing. Keeping sockets stable preserves inherited SSHORE_SOCKET and process
// discovery. Atomic name sidecars and lifetime locks resolve labels without
// contacting unrelated daemons. Status probes are advisory; the daemon still arbitrates attachment.
// Renaming does not rewrite pending approvals: their old name/PID must revalidate.

func sessionStatus(path string) (message, error) {
	c, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err != nil {
		return message{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if err := writeMessage(c, message{Type: "status", Action: "health"}); err != nil {
		return message{}, err
	}
	m, err := readMessage(c)
	if err == nil && m.Type != "session" {
		err = fmt.Errorf("invalid shell status")
	}
	if m.Name == "" {
		m.Name = strings.TrimSuffix(filepath.Base(path), ".sock")
	}
	return m, err
}

func resolveSession(name string) (string, error) {
	if err := validSession(name); err != nil {
		return "", err
	}
	dir, err := runtimeDir()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sock.lock") {
			continue
		}
		path := filepath.Join(dir, strings.TrimSuffix(e.Name(), ".lock"))
		live, err := sessionIdentityLive(path)
		if err != nil {
			return "", err
		}
		if !live {
			continue
		}
		label, err := readSessionName(path)
		if err != nil {
			return "", err
		}
		uid := ""
		if isShortTaskID(name) || validTaskUID(name) {
			uid, err = readSessionUID(path)
			if err != nil {
				return "", err
			}
		}
		if label == name || uid != "" && (uid == name || uid[:6] == name) {
			if found != "" {
				return "", fmt.Errorf("ambiguous shell name %s", name)
			}
			found = path
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
	path, err := resolveSession(old)
	if err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("shell %s does not exist", old)
	}

	other, err := resolveSession(name)
	if err != nil {
		return err
	}
	if other != "" && other != path {
		return fmt.Errorf("shell %s already exists", name)
	}
	old, err = readSessionName(path)
	if err != nil {
		return err
	}
	gate.Close() // The daemon takes the gate and rechecks the old label and destination.
	c, err := net.DialTimeout("unix", path, time.Second)
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
	gate, err := setupLock()
	if err != nil {
		return err
	}
	defer gate.Close()
	target, err := resolveSession(name)
	if err != nil {
		return err
	}
	if target != "" && target != s.path {
		return fmt.Errorf("shell %s already exists", name)
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
	reservation, err := reserveResourceName("shell", name)
	if err != nil {
		return err
	}
	defer reservation.Close()
	if err := writeSessionName(s.path, name); err != nil {
		return err
	}
	if s.nameLock != nil {
		s.nameLock.Close()
	}
	s.nameLock = reservation.owner
	reservation.owner = nil
	s.name = name
	renameShellTree(remoteTreeIdentity(s.instance), name)
	return nil
}

// The lifetime flock, not responsiveness, determines whether an identity owns
// its label. Never remove lock files: doing so could create two lock inodes.
func sessionIdentityLive(path string) (bool, error) {
	lock, err := openLock(path+".lock", true)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, lock.Close()
}

// Older daemons have no sidecar and use their immutable socket basename.
// New daemons publish the label before listening, then replace it atomically
// under the account setup lock when renamed. Stale sidecars are ignored unless
// the corresponding lifetime lock is held.
func readSessionName(path string) (string, error) {
	fd, err := unix.Open(path+".name", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return strings.TrimSuffix(filepath.Base(path), ".sock"), nil
	}
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path+".name")
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("invalid shell name file: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, 49))
	if err != nil {
		return "", err
	}
	name := string(data)
	if err := validSession(name); err != nil {
		return "", fmt.Errorf("invalid shell label at %s: %w", path, err)
	}
	return name, nil
}

func writeSessionName(path, name string) error {
	if err := validSession(name); err != nil {
		return err
	}
	return writeStateFile(path+".name", []byte(name))
}

func firstExpectedInstance(expected []string) string {
	if len(expected) > 0 {
		return expected[0]
	}
	return ""
}

func readSessionUID(path string) (string, error) {
	path += ".id"
	if err := privateStatePath(path, false); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		return "", err
	}
	uid := string(data)
	if !validTaskUID(uid) {
		return "", fmt.Errorf("invalid shell identity at %s", path)
	}
	return uid, nil
}

// Explicit kill resolves a name, or an account-wide confirmed instance, while holding
// the account setup lock. Only a discovered same-user daemon whose socket peer
// matches its PID is eligible. Reuse setup's process snapshot and pidfd signals
// (TERM, then bounded KILL) so PID reuse cannot redirect termination. No setup,
// installation, version migration, or name-wide signaling occurs on this path.

func killSession(name string, expected ...string) error {
	if err := validSession(name); err != nil {
		return err
	}
	gate, err := setupLock()
	if err != nil {
		return err
	}
	defer gate.Close()
	processes, err := findSessionDaemons("/proc")
	if err != nil {
		return err
	}
	var path string
	if len(expected) > 0 && expected[0] != "" {
		// Inventory spans runtime directories. Resolve the confirmed instance,
		// never a same-name session in the current directory.
		for _, candidate := range processes {
			if candidate.socket == "" {
				continue
			}
			m, err := sessionStatus(candidate.socket)
			if err == nil && m.Instance == expected[0] {
				if path != "" && path != candidate.socket {
					return fmt.Errorf("ambiguous shell instance; refresh with sshore list")
				}
				path = candidate.socket
			}
		}
	} else {
		path, err = resolveSession(name)
		if err != nil {
			return err
		}
	}
	if path == "" {
		return fmt.Errorf("shell %s does not exist or its instance changed", name)
	}
	var matches []daemonProcess
	for _, p := range processes {
		if p.socket == path {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("shell %s: expected one identifiable daemon, found %d; use sshore list", name, len(matches))
	}
	p := matches[0]
	start, err := processStart(p.pid)
	if err != nil {
		return err
	}
	v, err := executableVersion(fmt.Sprintf("/proc/%d/exe", p.pid))
	if err != nil {
		return fmt.Errorf("cannot verify shell version: %w", err)
	}
	if err := checkPeerVersion(v, "shell daemon"); err != nil {
		return err
	}
	if len(expected) > 0 && expected[0] != "" {
		m, err := sessionStatus(path)
		if err != nil {
			return err
		}
		if m.Instance != expected[0] {
			return fmt.Errorf("shell instance changed; refresh with sshore list")
		}
	}
	health := probeSessionDaemon(p, time.Now().Add(time.Second))
	if health.state != "attached" && health.state != "detached" && health.state != "starting" {
		return fmt.Errorf("shell %s is %s; refusing to signal an unconfirmed daemon", name, health.state)
	}
	if p.shared {
		m, err := sessionStatus(path)
		if err != nil {
			return err
		}
		if len(expected) > 0 && expected[0] != "" && m.Instance != expected[0] {
			return fmt.Errorf("shell instance changed; refresh with sshore list")
		}
		c, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if err = writeMessage(c, message{Type: "cancel", Instance: m.Instance, SoftwareVersion: version}); err != nil {
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
	if err := stopOldSessions([]setupProcess{{PID: p.pid, Start: start, Version: v}}, "cancelled by sshore cancel"); err != nil {
		return err
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

// Called after identity/signal preflight, and published only after successful
// teardown. Peer credentials are checked by probeSessionDaemon. No extra signals.
func cleanupExitReceipts(roots []setupProcess) map[string]string {
	result := map[string]string{}
	processes, _ := findSessionDaemons("/proc")
	for _, p := range processes {
		for _, root := range roots {
			if p.pid != root.PID {
				continue
			}
			if start, err := processStart(p.pid); err != nil || start != root.Start {
				continue
			}
			health := probeSessionDaemon(p, time.Now().Add(time.Second))
			if health.state != "attached" && health.state != "detached" && health.state != "starting" {
				continue
			}
			m, err := sessionStatus(p.socket)
			if err == nil {
				result[p.socket] = m.Name
			}
		}
	}
	return result
}

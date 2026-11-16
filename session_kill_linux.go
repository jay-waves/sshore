package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

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

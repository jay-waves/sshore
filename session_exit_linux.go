package main

// Exit receipts are best-effort diagnostics, not evidence of task cleanup. Each
// immutable socket identity has at most one receipt; newer lifetimes replace it.
// Keep at most 100 receipts for seven days in the private runtime directory.
// Atomic 0600 writes and bounded no-follow reads avoid partial/unbounded records.
// Abrupt SIGKILL, reboot or directory removal may leave no receipt. Explicit kill
// and verified update cleanup write receipts after successful process teardown,
// including old daemons that cannot report their own reasons.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

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

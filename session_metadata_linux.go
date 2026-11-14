package main

// Public names are mutable labels on immutable daemon/socket identities. Resolve
// labels under the account setup lock before creating, attaching, renaming or
// killing. Keeping sockets stable preserves inherited SSHORE_SOCKET and process
// discovery. Atomic name sidecars and lifetime locks resolve labels without
// contacting unrelated daemons. Status probes are advisory; the daemon still arbitrates attachment.
// Renaming does not rewrite pending approvals: their old name/PID must revalidate.

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

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
		err = fmt.Errorf("invalid session status")
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
		if label == name {
			if found != "" {
				return "", fmt.Errorf("ambiguous session name %s", name)
			}
			found = path
		}
	}
	return found, nil
}

func renameSession(old, name string) error {
	if err := validSession(name); err != nil {
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
		return fmt.Errorf("session %s does not exist", old)
	}

	other, err := resolveSession(name)
	if err != nil {
		return err
	}
	if other != "" && other != path {
		return fmt.Errorf("session %s already exists", name)
	}
	gate.Close() // The daemon takes the gate and rechecks the old label and destination.
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, message{Type: "rename", Name: name, Action: old, SoftwareVersion: version}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if m.Type != "renamed" {
		return fmt.Errorf("rename: %s", m.Error)
	}
	fmt.Fprintf(os.Stdout, "Renamed session %s to %s\n", old, name)
	return nil
}

func sessionLabel(m message, filename string) string {
	if validSession(m.Name) == nil {
		return m.Name
	}
	return cleanText(strings.TrimSuffix(filename, ".sock"))
}

func (s *session) rename(old, name string) error {
	if err := validSession(name); err != nil {
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
		return fmt.Errorf("session %s already exists", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || s.name != old {
		return fmt.Errorf("session changed or is exiting; refresh the session list")
	}
	if err := writeSessionName(s.path, name); err != nil {
		return err
	}
	s.name = name
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
		return "", fmt.Errorf("invalid session name file: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, 49))
	if err != nil {
		return "", err
	}
	name := string(data)
	if err := validSession(name); err != nil {
		return "", fmt.Errorf("invalid session label at %s: %w", path, err)
	}
	return name, nil
}

func writeSessionName(path, name string) error {
	if err := validSession(name); err != nil {
		return err
	}
	return writeStateFile(path+".name", []byte(name))
}

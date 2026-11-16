package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/tmux"
)

func localShellPath(env []string) string {
	path := localEnvironmentValue(env, "SHELL")
	if path == "" {
		path = "/bin/sh"
	}
	return path
}

func redrawLocalTerminal(t *tmux.Terminal) { tmux.RequestRedraw(t, t.PID()) }

func detachLocalDaemon(cmd *exec.Cmd) { detachResourceWorker(cmd) }

func spawnProcessIdentity(pid int) (string, error) {
	p, err := readSetupTask(pid)
	if err != nil {
		return "", err
	}
	if p.state == "Z" || p.state == "X" {
		return "", fmt.Errorf("parent shell exited")
	}
	return p.start, nil
}

func stopSpawnTerminal(pid int, start string) error {
	return stopOldSessions([]setupProcess{{PID: pid, Start: start}}, "spawn owner exited")
}

func detachResourceWorker(c *exec.Cmd)   { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func lockResourceState(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func prepareFTPPaths(o *ftpOptions) (func(), error) {
	cleanup := func() {}
	if o.mountpoint != "" {
		p, e := filepath.Abs(o.mountpoint)
		o.mountpoint = p
		return cleanup, e
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return cleanup, e
	}
	root := filepath.Join(home, "mnt")
	if e = os.MkdirAll(root, 0700); e != nil {
		return cleanup, e
	}
	// ~/mnt may already be shared for other user-managed mounts (mode 0755).
	// Require ownership and prevent other users from replacing mount directories.
	info, e := os.Lstat(root)
	if e != nil {
		return cleanup, e
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return cleanup, fmt.Errorf("mount parent must be a directory owned by you and not writable by others: %s", root)
	}
	o.mountpoint = filepath.Join(root, o.name)
	e = os.Mkdir(o.mountpoint, 0700)
	if os.IsExist(e) {
		return cleanup, files.ValidateMountpoint(o.mountpoint)
	}
	if e != nil {
		return cleanup, e
	}
	return func() { _ = os.Remove(o.mountpoint) }, nil
}

func resourceLockBusy(err error) bool { return errors.Is(err, unix.EWOULDBLOCK) }

// Inspect lifetime locks and labels, never contact the shells or signal tasks.
func occupiedShellNames() (map[string]bool, error) {
	names := map[string]bool{}
	dir, err := runtimeDir()
	if err != nil {
		return nil, err
	}
	dirs := map[string]bool{dir: true}
	processes, err := findSessionDaemons("/proc")
	if err != nil {
		return nil, err
	}
	for _, p := range processes {
		if p.socket != "" {
			dirs[filepath.Dir(p.socket)] = true
		}
	}
	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".sock.lock") {
				continue
			}
			path := filepath.Join(dir, strings.TrimSuffix(entry.Name(), ".lock"))
			live, err := sessionIdentityLive(path)
			if err != nil {
				return nil, err
			}
			if !live {
				continue
			}
			name, err := readSessionName(path)
			if err != nil {
				return nil, err
			}
			names[name] = true
		}
	}
	return names, nil
}

// Private validates ownership, permissions and path type without following symlinks.
func privateStatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("state path must be owned by you, private, and not a symlink: %s", path)
	}
	return nil
}

// Read existing OS identity without changing system configuration.
func systemMachineID() string {
	for _, path := range []string{"/var/lib/dbus/machine-id", "/etc/machine-id"} {
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

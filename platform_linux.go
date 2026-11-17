package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/tmux"
	"golang.org/x/sys/unix"
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
func lockResourceReadState(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
}
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

// Inventory is a read-only, exact-release endpoint. It never enters setup or
// installs software. Session data and peer inventory remain advisory snapshots.

func hostedShellInventory(detailed bool) (inventory, error) {
	inv := emptyInventory()
	inv.MachineID, inv.Hostname = localMachineIdentity()
	inv.ShellTree = knownShellTrees()
	dir, e := runtimeDir()
	if e != nil {
		return inv, e
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return inv, e
	}
	paths := map[string]bool{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sock") {
			paths[filepath.Join(dir, entry.Name())] = true
		}
	}
	processes, e := findSessionDaemons("/proc")
	if e != nil {
		return inv, e
	}
	for _, p := range processes {
		if p.socket == "" {
			inv.Errors = append(inv.Errors, fmt.Sprintf("shell daemon %d has no readable endpoint", p.pid))
		} else {
			paths[p.socket] = true
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	unavailable := func(path string) {
		name, err := readSessionName(path)
		if err != nil {
			name = strings.TrimSuffix(filepath.Base(path), ".sock")
		}
		uid, _ := readSessionUID(path)
		view := shellView{Name: name, Instance: uid, State: "unavailable"}
		if uid != "" {
			for _, n := range shellTreeFor(remoteTreeIdentity(uid)) {
				if n.Identity == remoteTreeIdentity(uid) {
					view.Lifetime, view.Temporary = n.Lifetime, n.Lifetime == "parent" || n.Lifetime == "heartbeat"
				}
			}
		}
		inv.Shells = append(inv.Shells, view)
	}
	action := "inventory"
	if detailed {
		action = ""
	}
	for _, path := range ordered {
		live, err := sessionIdentityLive(path)
		if err != nil {
			inv.Errors = append(inv.Errors, cleanText(err.Error()))
			continue
		}
		if !live {
			continue
		}
		c, e := net.DialTimeout("unix", path, time.Second)
		if e != nil {
			unavailable(path)
			inv.Errors = append(inv.Errors, "shell unavailable: "+cleanText(filepath.Base(path)))
			continue
		}
		c.SetDeadline(time.Now().Add(time.Second))
		if e = writeMessage(c, message{Type: "status", Action: action}); e != nil {
			c.Close()
			unavailable(path)
			inv.Errors = append(inv.Errors, "shell status request failed: "+cleanText(filepath.Base(path)))
			continue
		}
		m, e := readMessage(c)
		c.Close()
		if e != nil || m.Type != "session" || validSession(m.Name) != nil {
			unavailable(path)
			inv.Errors = append(inv.Errors, "invalid shell status: "+cleanText(filepath.Base(path)))
			continue
		}
		state := "detached"
		if m.Attached {
			state = "attached"
		}
		inv.Shells = append(inv.Shells, shellView{Lifetime: shellLifetime(m.Temporary, true), Temporary: m.Temporary, Instance: m.Instance, ID: shellTaskName(cleanText(m.Name), m.Temporary), Name: cleanText(m.Name), State: state, PID: m.PID, Age: m.AgeSeconds, Foreground: cleanText(m.Foreground), Directory: cleanText(m.Directory)})
	}
	for _, r := range readSessionExits(dir) {
		if time.Since(r.At) > 7*24*time.Hour {
			continue
		}
		inv.Exits = append(inv.Exits, exitView{Name: r.Name, Reason: r.Reason, Time: r.At})
	}
	return inv, nil
}

func localSessionInventory(details ...bool) (inventory, error) {
	detailed := len(details) > 0 && details[0]
	inv, e := hostedShellInventory(detailed)
	if e != nil {
		return inv, e
	}

	records, e := collectLocalResources("")
	if e != nil {
		return inv, e
	}
	inv.Resources = inspectResources(records)
	if detailed {
		inv.Resources = append(inv.Resources, collectResourceResults("", inv.Resources)...)
	}
	inv.Peers, e = collectPeerViews()
	return inv, e
}

func checkPlatform() error { return nil }

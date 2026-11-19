package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"golang.org/x/sys/unix"
)

func localShellPath(env []string) string {
	path := localEnvironmentValue(env, "SHELL")
	if path == "" {
		path = "/bin/sh"
	}
	return path
}

// Catch HUP for the entire account daemon lifetime, including intervals with
// no hosted shells. Do not use SIG_IGN, which would survive child exec.
func protectDaemonHangups() func() {
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	return func() { signal.Stop(hangups) }
}

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
	return stopOwnedProcesses([]setupProcess{{PID: pid, Start: start}})
}

func detachDaemon(c *exec.Cmd)           { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func lockResourceState(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }

func daemonProcessWaiter(_ int) (func(time.Time) error, func(), error) {
	// Releasing the account lock is sufficient for replacement on Linux.
	return nil, nil, nil
}

func prepareFTPPaths(o *ftpOptions) (func(), error) {
	cleanup := func() {}
	if o.Mountpoint != "" {
		p, e := absoluteLocalPath(o.Mountpoint)
		o.Mountpoint = p
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
	o.Mountpoint = filepath.Join(root, o.Name)
	e = os.Mkdir(o.Mountpoint, 0700)
	if os.IsExist(e) {
		return cleanup, files.ValidateMountpoint(o.Mountpoint)
	}
	if e != nil {
		return cleanup, e
	}
	return func() { _ = os.Remove(o.Mountpoint) }, nil
}

func resourceLockBusy(err error) bool { return errors.Is(err, unix.EWOULDBLOCK) }

// Names are an advisory snapshot; the daemon atomically arbitrates creation.

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
	if _, e := runtimeDir(); e != nil {
		return inv, e
	}
	detail := "inventory"
	if detailed {
		detail = ""
	}
	shells, e := hostedShells(detail)
	if e != nil {
		return inv, e
	}
	inv.Shells = append(inv.Shells, shells...)
	return inv, nil
}

func hostedExitHistory() []exitView {
	exits := []exitView{}
	dir, err := runtimeDir()
	if err != nil {
		return exits
	}
	for _, r := range recentSessionExits(dir, false) {
		exits = append(exits, exitView{Name: r.Name, Reason: r.Reason, Time: r.At})
	}
	return exits
}

func openHostedTransport(ctx context.Context, o options) (net.Conn, error) {
	path, err := resolveSession(o.session, o.instance)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	return dialHostedTask(ctx, path, time.Second)
}

func runHostedCommand(o options) (int, error) {
	switch o.command {
	case "attach":
		return runClient(o)
	case "kill":
		return 0, killSession(o.session, o.instance)
	case "rename":
		return 0, renameSession(o.session, o.newName, o.instance)
	default:
		return 2, usage("unsupported hosted shell action %s", o.command)
	}
}

func localSessionInventory(details ...bool) inventory {
	inv := collectManagedInventory(len(details) > 0 && details[0], false)
	inv.collectShellHistory()
	inv.collectPeers()
	return inv
}

func checkPlatform() error { return nil }

func resetRuntimeState(directories []string) error {
	current, err := runtimeLocation()
	if err != nil {
		return err
	}
	directories = append(directories, current)
	seen := map[string]bool{}
	for _, dir := range directories {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("invalid runtime directory")
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
			if name == ".exit.lock" || strings.HasSuffix(name, ".exit.json") || validTaskUID(strings.TrimSuffix(name, ".sock")) && strings.HasSuffix(name, ".sock") {
				if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
	}
	return nil
}

func spawnProcessName(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return strings.TrimSpace(string(data)), err
}

func runLocalInstaller(target string) error {
	if target == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		target = filepath.Join(filepath.Dir(executable), "sshore")
	}
	cmd := exec.Command("sh", "-c", linuxInstaller, "install.sh", "--path", target)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

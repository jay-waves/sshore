package main

import (
	"context"

	"fmt"
	"net"
	"os"

	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/files"
)

func stopSpawnTerminal(pid int, start string) error {
	return stopOwnedProcesses([]setupProcess{{PID: pid, Start: start}})
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

// Read existing OS identity without changing system configuration.
func systemMachineID() string {
	for _, path := range []string{"/var/lib/dbus/machine-id", "/etc/machine-id"} {
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

func openHostedTransport(ctx context.Context, o options) (net.Conn, error) {
	id, err := resolveSession(o.session, o.instance)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	return dialHostedTask(ctx, id, time.Second)
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

func localSessionInventory() inventory {
	inv, err := daemonListTasks(taskListRequest{})
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	return inv
}

func checkPlatform() error { return nil }

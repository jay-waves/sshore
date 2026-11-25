package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/sys"
)

func reportBootstrapCgroup() {
	if err := sys.ProbeShellCgroup(); err != nil {
		fmt.Fprintf(os.Stderr, "Linux shell cgroup unavailable: %v. Mounts and forwards remain available.\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "Linux shell cgroup available.")
}

func daemonTaskCgroup() (string, error) { return sys.DaemonTaskCgroup() }

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

func checkPlatform() error { return nil }

// Older releases wrote receipts beside their former sockets. Read those
// observations until they expire and allow reset to clear their known files.
func legacyShellRuntime() string {
	if dir := os.Getenv("SSHORE_RUNTIME_DIR"); dir != "" {
		return dir
	}
	if root := os.Getenv("XDG_RUNTIME_DIR"); root != "" {
		return filepath.Join(root, "sshore")
	}
	return fmt.Sprintf("/tmp/sshore-%d", os.Geteuid())
}

func resetHostedExitState(account string, directories []string) error {
	// Reset holds the account owner lock, so no daemon can write new receipts.
	directories = append(directories, account)
	// A stale runtime override cannot block reset of current state.
	if dir := legacyShellRuntime(); filepath.IsAbs(dir) && privateStatePath(dir, true) == nil {
		directories = append(directories, dir)
	}
	for _, dir := range directories {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("invalid legacy runtime directory")
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
			id, receipt := strings.CutSuffix(name, ".exit.json")
			socketID, socket := strings.CutSuffix(name, ".sock")
			if receipt && validTaskUID(strings.TrimSuffix(id, ".sock")) || socket && validTaskUID(socketID) || name == ".exit.lock" {
				if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
	}
	return nil
}

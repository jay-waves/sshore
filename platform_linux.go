package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

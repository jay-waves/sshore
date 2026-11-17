package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Reset is local administrative maintenance, independent of normal release
// matching and daemon metadata. Data is removed only after the owner exits.
func runReset() error {
	if os.Getenv("SSHORE_LOCAL_SESSION") != "" || os.Getenv("SSHORE_SOCKET") != "" {
		return fmt.Errorf("run reset outside managed shells; reset ends their tasks")
	}
	root, err := statePath("")
	if err != nil {
		return err
	}
	cache, err := resetCachePath()
	if err != nil {
		return err
	}
	account, err := daemonDirectory()
	if err != nil {
		return err
	}
	for _, path := range []string{root, cache, filepath.Dir(account)} {
		if err := validateResetDirectory(path); err != nil {
			return err
		}
	}
	setup, err := setupLock()
	if err != nil {
		return err
	}
	defer setup.Close()
	startup, err := daemonFileLock("startup")
	if err != nil {
		return err
	}
	defer startup.Close()
	reply, err := daemonAdministration(context.Background(), "snapshot", nil)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := requestDaemonShutdown(reply.Snapshot); err != nil {
			return fmt.Errorf("reset stopped; application state retained: %w", err)
		}
	}
	owner, err := daemonFileLock("owner")
	if err != nil {
		return err
	}
	defer owner.Close()
	// Keep lock inodes stable: a waiting client must never acquire a deleted lock
	// while another client starts a replacement daemon under a different inode.
	keep := map[string]bool{
		setup.Name(): true, startup.Name(): true, owner.Name(): true,
	}
	if err := resetRuntimeState(reply.Runtime); err != nil {
		return err
	}
	if os.Getenv("SSHORE_STATE_DIR") != "" {
		// A custom state directory may contain user files; clear only sshore's
		// known entries and leave unrelated contents alone.
		for _, name := range []string{"identity", "shells", "shell-tree", "connections", "approvals", "requests", "mounts", "forwards", "peers", "names", "activity"} {
			if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
				return err
			}
		}
	} else if err := removeResetData(root, keep); err != nil {
		return err
	}
	if err := removeResetData(cache, keep); err != nil {
		return err
	}
	if filepath.Dir(account) == root && os.Getenv("SSHORE_STATE_DIR") != "" {
		if err := removeResetData(account, keep); err != nil {
			return err
		}
	} else if err := removeResetData(filepath.Dir(account), keep); err != nil {
		return err
	}
	fmt.Println("Reset complete; local tasks ended and application state and caches cleared.")
	return nil
}

// State/cache roots must be dedicated directories, never a home directory,
// filesystem root, or an ancestor containing the installed executable.
func validateResetDirectory(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if absolute == filepath.Dir(absolute) || absolute == home || resetContains(absolute, executable) {
		return fmt.Errorf("refusing to reset unsafe state directory: %s", path)
	}
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("reset requires a real application directory: %s", path)
	}
	return privateStatePath(absolute, true)
}

func resetContains(parent, path string) bool {
	for {
		if parent == path {
			return true
		}
		next := filepath.Dir(path)
		if next == path {
			return false
		}
		path = next
	}
}

func removeResetData(root string, keep map[string]bool) error {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if keep[path] {
			continue
		}
		containsLock := false
		for lock := range keep {
			if resetContains(path, lock) {
				containsLock = true
				break
			}
		}
		if containsLock {
			if err := removeResetData(path, keep); err != nil {
				return err
			}
		} else if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	// Empty roots have no remaining state; locked coordination directories stay.
	_ = os.Remove(root)
	return nil
}

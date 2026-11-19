package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

var resetCachePath = func() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "sshore"), nil
}

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
	account, err := daemonLocation()
	if err != nil {
		return err
	}
	accountRoot := filepath.Dir(account)
	for _, path := range []*string{&root, &cache, &accountRoot, &account} {
		checked, err := validateResetDirectory(*path)
		if err != nil {
			return err
		}
		*path = checked
	}
	custom := os.Getenv("SSHORE_STATE_DIR") != ""
	if !custom {
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(filepath.Join(base, "sshore"), root)
		custom = err != nil || rel != "."
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
	keep := make(map[string]bool)
	for _, lock := range []*os.File{setup, startup, owner} {
		parent, err := filepath.EvalSymlinks(filepath.Dir(lock.Name()))
		if err != nil {
			return err
		}
		keep[filepath.Join(parent, filepath.Base(lock.Name()))] = true
	}
	if err := resetRuntimeState(reply.Runtime); err != nil {
		return err
	}
	if custom {
		// A custom state directory may contain user files; clear only sshore's
		// known entries and leave unrelated contents alone.
		for _, name := range []string{"identity", "shells", "shell-tree", "connections", "approvals", "requests", "mounts", "mount-history", "forwards", "peers", "names", "activity"} {
			if err := removeResetEntry(filepath.Join(root, name), keep); err != nil {
				return err
			}
		}
		keep[root] = true
	} else if err := removeResetData(root, keep); err != nil {
		return err
	}
	if err := removeResetData(cache, keep); err != nil {
		return err
	}
	// The account directory has a fixed layout. Its other files and the
	// coordination lock files are outside reset's removal set.
	for _, name := range []string{"daemon.json", "local.log"} {
		if err := os.Remove(filepath.Join(account, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	fmt.Println("Reset complete; local tasks ended and application state and caches cleared.")
	return nil
}

// State/cache roots must be dedicated directories, never a home directory,
// filesystem root, or an ancestor containing the installed executable.
func validateResetDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	sameHome := resetContains(absolute, home) && resetContains(home, absolute)
	if absolute == filepath.Dir(absolute) || sameHome || resetContains(absolute, executable) {
		return "", fmt.Errorf("refusing to reset unsafe state directory: %s", path)
	}
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		return absolute, nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("reset requires a real application directory: %s", path)
	}
	if homeInfo, err := os.Stat(home); err == nil && os.SameFile(info, homeInfo) {
		return "", fmt.Errorf("refusing to reset unsafe state directory: %s", path)
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	realExecutable, err := filepath.EvalSymlinks(executable)
	if err == nil && resetContains(real, realExecutable) || real == filepath.Dir(real) {
		return "", fmt.Errorf("refusing to reset unsafe state directory: %s", path)
	}
	return real, privateStatePath(absolute, true)
}

func resetContains(parent, path string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && filepath.IsLocal(relative)
}

func removeResetData(root string, keep map[string]bool) error {
	for protected := range keep {
		if resetContains(protected, root) {
			return nil
		}
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removeResetEntry(filepath.Join(root, entry.Name()), keep); err != nil {
			return err
		}
	}
	// Empty roots have no remaining state; locked coordination directories stay.
	_ = os.Remove(root)
	return nil
}

func removeResetEntry(path string, keep map[string]bool) error {
	for protected := range keep {
		if resetContains(protected, path) {
			return nil
		}
	}
	for protected := range keep {
		if resetContains(path, protected) {
			return removeResetData(path, keep)
		}
	}
	return os.RemoveAll(path)
}

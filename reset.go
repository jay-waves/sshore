package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jay-waves/sshore/internal/sys"
)

var resetCachePath = func() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	// Windows installs the executable in LocalAppData/sshore/bin alongside the cache.
	if runtime.GOOS == "windows" {
		return filepath.Join(root, "sshore", "releases"), nil
	}
	return filepath.Join(root, "sshore"), nil
}

type resetPaths struct {
	state, cache, account string
	customState           bool
}

func requireLocalMaintenance(command string) error {
	if deployedStateRoot() != "" {
		return fmt.Errorf("%s applies to the installed client", command)
	}
	if os.Getenv("SSHORE_LOCAL_SESSION") != "" || os.Getenv("SSHORE_SOCKET") != "" {
		return fmt.Errorf("run %s outside managed shells; it ends their tasks", command)
	}
	return nil
}

// Resolve the running daemon's state directory before shutdown removes its record.
func prepareReset() (resetPaths, error) {
	root, err := statePath("")
	if err != nil {
		return resetPaths{}, err
	}
	cache, err := resetCachePath()
	if err != nil {
		return resetPaths{}, err
	}
	account, err := daemonLocation()
	if err != nil {
		return resetPaths{}, err
	}
	accountRoot := filepath.Dir(account)
	for _, path := range []*string{&root, &cache, &accountRoot, &account} {
		checked, err := validateResetDirectory(*path)
		if err != nil {
			return resetPaths{}, err
		}
		*path = checked
	}
	custom := os.Getenv("SSHORE_STATE_DIR") != ""
	if !custom {
		base, err := os.UserConfigDir()
		if err != nil {
			return resetPaths{}, err
		}
		rel, err := filepath.Rel(filepath.Join(base, "sshore"), root)
		custom = err != nil || rel != "."
	}
	return resetPaths{state: root, cache: cache, account: account, customState: custom}, nil
}

func runReset() error {
	if err := requireLocalMaintenance("reset"); err != nil {
		return err
	}
	startup, err := daemonFileLock("startup")
	if err != nil {
		return err
	}
	defer startup.Close()
	paths, err := prepareReset()
	if err != nil {
		return err
	}
	snapshot, err := readDaemonSnapshot(context.Background())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := requestDaemonShutdown(snapshot); err != nil {
			return fmt.Errorf("reset stopped; application state retained: %w", err)
		}
	}
	owner, err := daemonFileLock("owner")
	if err != nil {
		return err
	}
	defer owner.Close()
	if err := paths.clear(startup, owner); err != nil {
		return err
	}
	fmt.Println("Reset complete; local tasks ended and application state and caches cleared.")
	return nil
}

// clear runs with startup and owner locked.
func (p resetPaths) clear(locks ...*sys.FileLock) error {
	// Keep lock inodes stable: a waiting client must never acquire a deleted lock
	// while another client starts a replacement daemon under a different inode.
	keep := make(map[string]bool)
	for _, lock := range locks {
		parent, err := filepath.EvalSymlinks(filepath.Dir(lock.Path()))
		if err != nil {
			return err
		}
		keep[filepath.Join(parent, filepath.Base(lock.Path()))] = true
	}
	if p.customState {
		// A custom state directory may contain user files; clear only sshore's
		// known entries and leave unrelated contents alone.
		for _, name := range []string{"identity", "shells", "shell-tree", "connections", "approvals", "requests", "mounts", "mount-history", "forwards", "peers", "names", "activity"} {
			if err := removeResetEntry(filepath.Join(p.state, name), keep); err != nil {
				return err
			}
		}
		// Preserve custom state if the cache contains it. A dedicated cache
		// below the state directory can still be cleared normally.
		if resetContains(p.cache, p.state) {
			keep[p.state] = true
		}
	} else if err := removeResetData(p.state, keep); err != nil {
		return err
	}
	if err := removeResetData(p.cache, keep); err != nil {
		return err
	}
	// The account directory has a fixed layout. Its other files and the
	// coordination lock files are outside reset's removal set.
	for _, name := range []string{"daemon.json", "local.log"} {
		if err := os.Remove(filepath.Join(p.account, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
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
	return real, sys.PrivateStatePath(absolute, true)
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

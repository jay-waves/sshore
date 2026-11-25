//go:build linux || (darwin && arm64)

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
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

func detachDaemon(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func completeDaemonShutdown(_ context.Context, _ int, shutdown func() error) error {
	// Releasing the account lock is sufficient for replacement on Unix.
	return shutdown()
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

func validateInstallTarget(target string) error {
	dirInfo, err := os.Stat(filepath.Dir(target))
	if err != nil {
		return err
	}
	owner, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || dirInfo.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("automatic update requires a user-owned directory without group/world write access; install manually")
	}
	if info, e := os.Lstat(target); e == nil {
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() {
			return fmt.Errorf("automatic update requires a user-owned regular executable; install manually")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	return nil
}

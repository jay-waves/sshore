package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jay-waves/sshore/internal/release"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

//go:embed install.sh
var linuxInstaller string

//go:embed install.ps1
var windowsInstaller string

func runInstall(parsed cliArguments) (int, error) {
	var target string
	if len(parsed.operands) != 0 {
		var local bool
		target, local = strings.CutPrefix(parsed.operands[0], "here:")
		if !local {
			return 2, usage("install applies to this machine; use here: or here:PATH")
		}
	}
	if target != "" && (!filepath.IsAbs(target) || strings.ContainsAny(target, "\x00\r\n")) {
		return 2, usage("installation path must be absolute and contain no line breaks")
	}
	if parsed.has("--apply") {
		if target == "" {
			return 2, usage("applying a prepared release requires here:PATH")
		}
		return 0, installRelease(target)
	}
	if runtime.GOOS == "linux" && term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintln(os.Stderr, termui.StatusLine(termui.ColorEnabled(os.Stderr), termui.Info,
			"Resetting and reinstalling sshore · "+cleanText("here:"+target)))
	}
	err := release.RunInstaller(target, linuxInstaller, windowsInstaller)
	if err == nil && runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, termui.StatusLine(termui.ColorEnabled(os.Stderr), termui.Info, "Continue setup in the installer window."))
	}
	return 0, err
}

// Serialize executable checks and replacement across the account.
func setupLock() (*sys.FileLock, error) {
	owner, err := daemonDirectory()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(owner)
	if err := sys.PrivateStatePath(dir, true); err != nil {
		return nil, err
	}
	return tryPrivateFileLock(filepath.Join(dir, "setup.lock"))
}

func reportInstallCgroup() {
	if runtime.GOOS != "linux" {
		return
	}
	if err := sys.ProbeShellCgroup(); err != nil {
		fmt.Fprintf(os.Stderr, "Linux shell cgroup unavailable: %v. Mounts and forwards remain available.\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "Linux shell cgroup available.")
}

func installRelease(target string) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("installation target must be absolute")
	}
	reportInstallCgroup()
	gate, err := setupLock()
	if err != nil {
		return err
	}
	defer gate.Close()
	startup, err := daemonFileLock("startup")
	if err != nil {
		return err
	}
	defer startup.Close()
	var installed string
	previous, err := os.Lstat(target)
	if err == nil {
		if !previous.Mode().IsRegular() {
			return fmt.Errorf("installation target must be a regular executable: %s", target)
		}
		installed, err = release.ExecutableVersion(target)
		order, known := release.Compare(installed, version)
		if err != nil || !known {
			return fmt.Errorf("unknown installed release; update manually")
		}
		if order > 0 {
			return fmt.Errorf("installed sshore %s is newer than %s", installed, version)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	record, err := existingDaemonRecord()
	running := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if running && record.Version != installed {
		return fmt.Errorf("stop the local daemon using its sshore %s client before installing", cleanText(record.Version))
	}

	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	if err := sys.ValidateInstallTarget(target); err != nil {
		return err
	}
	paths, err := prepareReset("install")
	if err != nil {
		return err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return err
	}
	for _, root := range []string{paths.state, paths.cache, paths.account} {
		if resetContains(root, filepath.Join(dir, filepath.Base(target))) {
			return fmt.Errorf("installation target is inside the sshore reset directory %s", root)
		}
	}
	staged, err := release.StageReplacement(target, version)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	if err := release.VerifyTarget(target, previous); err != nil {
		return err
	}
	if installed != "" {
		if err := stopInstalledDeployments(target, installed); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "Installing will end this account's sshore tasks, clear its identity, history and caches, and reinstall sshore %s at %s.\n", version, cleanText(target))
	if running {
		command := exec.Command(target, "kill", "--all")
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("stopping the local sshore %s daemon: %w", installed, err)
		}
	}
	// A direct daemon invocation can race even while we hold the startup lock.
	owner, err := daemonFileLock("owner")
	if err != nil {
		return fmt.Errorf("cannot take ownership for installation: %w", err)
	}
	defer owner.Close()
	if err := release.VerifyTarget(target, previous); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("replacing the executable before state cleanup: %w", err)
	}
	if err := paths.clear(gate, startup, owner); err != nil {
		return fmt.Errorf("installed sshore %s, but reset did not finish: %w; retry install", version, err)
	}
	if installed != "" {
		if err := forgetDeploymentHistory(installed); err != nil {
			return fmt.Errorf("installed sshore %s, but could not clear deployment history: %w", version, err)
		}
	}
	return nil
}

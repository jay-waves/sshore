package main

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

//go:embed install.sh
var linuxInstaller string

const installMatchingReleaseHelp = "Install the same full release on both machines.\n" +
	"Linux installer: https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh\n" +
	"Windows installer: https://raw.githubusercontent.com/jay-waves/sshore/main/install.ps1"

func runBootstrap(parsed cliArguments) (int, error) {
	var target string
	if len(parsed.operands) != 0 {
		var local bool
		target, local = strings.CutPrefix(parsed.operands[0], "here:")
		if !local {
			return 2, usage("bootstrap installs on this machine; use here: or here:PATH")
		}
	}
	if target != "" && (!filepath.IsAbs(target) || strings.ContainsAny(target, "\x00\r\n")) {
		return 2, usage("installation path must be absolute and contain no line breaks")
	}
	if parsed.has("--apply") {
		if target == "" {
			return 2, usage("applying a prepared release requires here:PATH")
		}
		return 0, bootstrapRelease(target)
	}
	if runtime.GOOS == "linux" && term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintln(os.Stderr, termui.StatusLine(termui.ColorEnabled(os.Stderr), termui.Info,
			"Resetting and reinstalling sshore · "+cleanText("here:"+target)))
	}
	return 0, runLocalInstaller(target)
}

func executableVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	return parseExecutableVersion(out)
}

func parseExecutableVersion(out []byte) (string, error) {
	line := strings.TrimSpace(string(out))
	if !strings.HasPrefix(line, "sshore ") || strings.ContainsAny(line, "\r\n") {
		return "", fmt.Errorf("unrecognized version response")
	}
	return strings.TrimPrefix(line, "sshore "), nil
}

// Serialize executable checks and replacement across the account.
func setupLock() (*sys.FileLock, error) {
	owner, err := daemonDirectory()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(owner)
	if err := privateStatePath(dir, true); err != nil {
		return nil, err
	}
	return tryPrivateFileLock(filepath.Join(dir, "setup.lock"))
}

func requireInstalledRelease(target string) error {
	installed, err := executableVersion(target)
	if err != nil {
		return fmt.Errorf("cannot verify installed sshore %s at %q.\n%s", version, target, installMatchingReleaseHelp)
	}
	if err := checkPeerVersion(installed, "installed executable"); err != nil {
		return err
	}
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Version != version {
		return fmt.Errorf("account daemon runs sshore %s. Use that release to run sshore kill --all and wait for it to exit before using %s", record.Version, version)
	}
	return nil
}

// Compare numeric components without overflowing on large release coordinates.
func compareReleases(a, b string) (int, bool) {
	aa, ok := releaseParts(a)
	if !ok {
		return 0, false
	}
	bb, ok := releaseParts(b)
	if !ok {
		return 0, false
	}
	for i := range aa {
		if order := cmp.Or(cmp.Compare(len(aa[i]), len(bb[i])), strings.Compare(aa[i], bb[i])); order != 0 {
			return order, true
		}
	}
	return 0, true
}

func bootstrapRelease(target string) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("installation target must be absolute")
	}
	reportBootstrapCgroup()
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
		installed, err = executableVersion(target)
		order, known := compareReleases(installed, version)
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
	if running {
		order, known := compareReleases(record.Version, version)
		if !known || order > 0 {
			return fmt.Errorf("cannot update account daemon release %q to %s", cleanText(record.Version), version)
		}
	}

	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	if err := validateInstallTarget(target); err != nil {
		return err
	}
	paths, err := prepareReset("bootstrap")
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
	staged, err := stageReplacement(target)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	stagedVersion, err := executableVersion(staged)
	if err != nil {
		return fmt.Errorf("prepared executable verification failed: %w", err)
	}
	if stagedVersion != version {
		return fmt.Errorf("prepared executable reports sshore %q, expected %q", stagedVersion, version)
	}
	if err := verifyInstallTarget(target, previous); err != nil {
		return err
	}
	if runtime.GOOS == "linux" && installed != "" && installed != version {
		if err := os.Link(target, target+"-"+installed); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("back up old executable: %w", err)
		}
	}
	fmt.Fprintf(os.Stderr, "Bootstrap will end this account's sshore tasks, clear its identity, history and caches, and reinstall sshore %s at %s.\n", version, cleanText(target))
	if running {
		snapshot, err := daemonAdminRequest(context.Background(), record, daemonSnapshotRoute, nil)
		if err != nil {
			return fmt.Errorf("reading account daemon snapshot: %w", err)
		}
		hostname, _ := os.Hostname()
		if err := confirmDaemonShutdown(hostname, snapshot, nil); err != nil {
			return err
		}
		if err := requestDaemonShutdown(snapshot); err != nil {
			return fmt.Errorf("stopping the account daemon: %w", err)
		}
	}
	// A direct daemon invocation can race even while we hold the startup lock.
	owner, err := daemonFileLock("owner")
	if err != nil {
		return fmt.Errorf("cannot take ownership for installation: %w", err)
	}
	defer owner.Close()
	if err := verifyInstallTarget(target, previous); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("replacing the executable before state cleanup: %w", err)
	}
	if err := paths.clear(gate, startup, owner); err != nil {
		return fmt.Errorf("installed sshore %s, but reset did not finish: %w; retry bootstrap", version, err)
	}
	return nil
}

func verifyInstallTarget(path string, previous os.FileInfo) error {
	current, err := os.Lstat(path)
	if os.IsNotExist(err) && previous == nil {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if previous == nil || current == nil || !os.SameFile(previous, current) {
		return fmt.Errorf("installation target changed; restart installation")
	}
	return nil
}

func stageReplacement(target string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	source, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer source.Close()
	return stageSiblingFile(target, ".sshore-update-*.exe", func(file *os.File) error {
		if _, err := io.Copy(file, source); err != nil {
			return err
		}
		return file.Chmod(0755)
	})
}

package main

import (
	"cmp"
	"context"
	_ "embed"
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
	if running && record.Version != installed {
		return fmt.Errorf("stop the local daemon using its sshore %s client before installing", cleanText(record.Version))
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
	if installed != "" {
		if err := stopInstalledDeployments(target, installed); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "Bootstrap will end this account's sshore tasks, clear its identity, history and caches, and reinstall sshore %s at %s.\n", version, cleanText(target))
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
	if err := verifyInstallTarget(target, previous); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("replacing the executable before state cleanup: %w", err)
	}
	if err := paths.clear(gate, startup, owner); err != nil {
		return fmt.Errorf("installed sshore %s, but reset did not finish: %w; retry bootstrap", version, err)
	}
	if installed != "" {
		if err := forgetDeploymentHistory(installed); err != nil {
			return fmt.Errorf("installed sshore %s, but could not clear deployment history: %w", version, err)
		}
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

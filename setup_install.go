package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
)

type setupState struct {
	Installed string
	Daemon    *daemonSnapshot
	Processes []setupProcess
	Manual    bool
}

func executableVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	if !strings.HasPrefix(line, "sshore ") || strings.ContainsAny(line, "\r\n") {
		return "", fmt.Errorf("unrecognized version response")
	}
	return strings.TrimPrefix(line, "sshore "), nil
}

// One lock per account, independent of custom runtime directories. Existing
// attached sessions remain usable while another client reviews the prompt.
func setupLock() (*os.File, error) {
	owner, err := daemonDirectory()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(owner)
	if err := privateStatePath(dir, true); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "setup.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = privateStatePath(file.Name(), false); err == nil {
		err = lockResourceState(file)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func inspectSetup(target string, includeMatching bool) (setupState, error) {
	var state setupState
	if _, err := os.Lstat(target); err == nil {
		state.Installed, err = executableVersion(target)
		if err != nil {
			state.Installed = "unknown"
			state.Manual = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	if state.Installed != "" {
		order, known := compareReleaseVersions(state.Installed, version)
		if known && order > 0 {
			return state, fmt.Errorf("remote executable %s is newer than client %s; update the local client", state.Installed, version)
		}
		if !known {
			state.Manual = true
		}
	}
	snapshot, err := daemonAdmin(context.Background(), "snapshot", nil)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("account daemon unavailable; no update performed: %w", err)
	}
	order, known := compareReleaseVersions(snapshot.Version, version)
	if known && order > 0 {
		return state, fmt.Errorf("account daemon %s is newer than client %s; update the local client", snapshot.Version, version)
	}
	if !known {
		state.Manual = true
	}
	if snapshot.Version != version || state.Installed != version && len(snapshot.Tasks) > 0 || includeMatching {
		state.Daemon = &snapshot
		state.Processes = append(state.Processes, setupProcess{Name: "daemon", Version: snapshot.Version})
		for _, task := range snapshot.Tasks {
			state.Processes = append(state.Processes, setupProcess{Name: task.Name, Version: snapshot.Version, Start: task.ID})
		}
	}
	return state, nil
}

// The verified release coordinates both standalone installers and SSH setup.
// Preparation never ends work; only a confirmed, rechecked snapshot can do that.
func installRelease(target string) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("installation target must be absolute")
	}
	lock, err := setupLock()
	if err != nil {
		return err
	}
	state, err := inspectSetup(target, true)
	if err != nil {
		lock.Close()
		return err
	}
	if state.Manual {
		lock.Close()
		return fmt.Errorf("unknown installed or daemon version; update manually; no tasks ended")
	}
	staged, err := prepareReplacement(target)
	lock.Close()
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	for {
		if state.Daemon != nil && len(state.Daemon.Tasks) > 0 && (os.Getenv("SSHORE_LOCAL_SESSION") != "" || os.Getenv("SSHORE_SOCKET") != "") {
			return fmt.Errorf("run the installer outside managed shells; no tasks ended")
		}
		if state.Installed != "" && state.Installed != version || state.Daemon != nil && len(state.Daemon.Tasks) > 0 {
			processes := state.Processes
			action, err := chooseSetupAction(message{Action: "update", Error: "Prepared sshore " + version + ". Updating ends the listed tasks; unsaved work may be lost.", SetupProcesses: processes}, &options{}, installerInput)
			if err != nil {
				return err
			}
			if action != "update" {
				return fmt.Errorf("installation cancelled; no tasks ended")
			}
		}
		lock, err := setupLock()
		if err != nil {
			return err
		}
		current, err := inspectSetup(target, true)
		if err != nil {
			lock.Close()
			return err
		}
		if !reflect.DeepEqual(state, current) {
			lock.Close()
			state = current
			if state.Manual {
				return fmt.Errorf("installation changed; update manually; no tasks ended")
			}
			fmt.Fprintln(os.Stderr, "Tasks changed; review again.")
			continue
		}
		// Revalidate the prepared executable under the account setup lock.
		if v, err := executableVersion(staged); err != nil || v != version {
			lock.Close()
			return fmt.Errorf("prepared executable verification failed; no tasks ended")
		}
		if state.Daemon != nil {
			err = requestDaemonShutdown(*state.Daemon)
		}
		if err == nil {
			err = os.Rename(staged, target)
		}
		lock.Close()
		if err != nil {
			return fmt.Errorf("installation stopped: %w", err)
		}
		return nil
	}
}

// Shell-script downloads may own stdin. Read approval from the controlling
// terminal on Linux; Windows' installer already runs in the invoking console.
type installerPrompt struct {
	reader *bufio.Reader
	close  func() error
}

func (p *installerPrompt) Read(prompt string, _ bool) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := p.reader.ReadString('\n')
	return strings.TrimSpace(line), err
}
func (p *installerPrompt) Close() error { return p.close() }
func installerInput() (setupInput, error) {
	if runtime.GOOS == "linux" {
		file, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("update confirmation requires a terminal; no tasks ended: %w", err)
		}
		return &installerPrompt{reader: bufio.NewReader(file), close: file.Close}, nil
	}
	return &installerPrompt{reader: bufio.NewReader(os.Stdin), close: func() error { return nil }}, nil
}

// Copy and verify in the destination filesystem before any task is stopped.
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
	file, err := os.CreateTemp(filepath.Dir(target), ".sshore-update-*.exe")
	if err != nil {
		return "", err
	}
	path := file.Name()
	success := false
	defer func() {
		file.Close()
		if !success {
			os.Remove(path)
		}
	}()
	if _, err = io.Copy(file, source); err != nil {
		return "", err
	}
	if err = file.Chmod(0755); err != nil {
		return "", err
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	if v, err := executableVersion(path); err != nil || v != version {
		return "", fmt.Errorf("prepared executable verification failed: %v", err)
	}
	success = true
	return path, nil
}

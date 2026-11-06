package main

// Managed setup uses one lock per remote account, independent of runtime paths.
// Inspect the installed program and visible same-user session daemons; exclude
// preparation helpers and transport processes. Require an exact full release,
// reject newer versions without downgrading, and restrict unknown versions to
// manual recovery. Missing software can install without confirmation only when
// there are no incompatible sessions. Existing older state requires typed update.
// After confirmation, reacquire the account lock, compare the process snapshot,
// and prepare/verify a replacement on the target filesystem before stopping tasks.
// Changed state requires a fresh prompt. Process identity checks and signaling
// live in setup_process_linux.go. Replacement is not an in-place daemon upgrade.
// Recovery keeps the authenticated transport but uses a transient PTY, removes
// helper socket inheritance and sets SSHORE_DISABLED. Approved operations are
// skipped after an update or in recovery; reconnect to restore managed features.
// This product restriction is not a sandbox for arbitrary remote executables.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type setupState struct {
	Installed string
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
	root, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, ".sshore")
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := privateState(dir, true); err != nil {
		return nil, err
	}
	return openLock(filepath.Join(dir, "setup.lock"), false)
}

func inspectSetup(target string) (setupState, error) {
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
		order, known := compareVersions(state.Installed, version)
		if known && order > 0 {
			return state, fmt.Errorf("remote executable %s is newer than client %s; update the local client", state.Installed, version)
		}
		if !known {
			state.Manual = true
		}
	}
	processes, err := findSessionDaemons("/proc")
	if err != nil {
		return state, err
	}
	for _, p := range processes {
		start, err := processStart(p.pid)
		if err != nil {
			continue
		} // process exited during the snapshot
		v, err := executableVersion(fmt.Sprintf("/proc/%d/exe", p.pid))
		if err != nil {
			v = "unknown"
			state.Manual = true
		}
		order, known := compareVersions(v, version)
		if known && order > 0 {
			return state, fmt.Errorf("remote session %s (PID %d) runs newer version %s; update local client %s", p.name, p.pid, v, version)
		}
		if !known {
			state.Manual = true
		}
		if v != version {
			state.Processes = append(state.Processes, setupProcess{PID: p.pid, Name: p.name, Version: v, Start: start})
		}
	}
	return state, nil
}

func bootstrap(target, mode, name string) (result error) {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("setup target must be absolute")
	}
	if mode != "bridge" && mode != "resume" && mode != "sessions" {
		return fmt.Errorf("invalid setup mode")
	}
	if mode == "resume" || mode == "bridge" && name != "" {
		if err := validSession(name); err != nil {
			return err
		}
	}
	// Fatal setup replies are framed; no human text ever enters protocol stdout.
	defer func() {
		if result != nil && mode != "sessions" {
			_ = writeMessage(os.Stdout, message{Type: "fatal", SoftwareVersion: version, Error: result.Error()})
		}
	}()
	updated := false
	changed := false
	for {
		lock, err := setupLock()
		if err != nil {
			return err
		}
		state, err := inspectSetup(target)
		if err != nil {
			lock.Close()
			return err
		}
		ready := state.Installed == version && len(state.Processes) == 0 && !state.Manual
		missing := state.Installed == "" && len(state.Processes) == 0 && !state.Manual
		if ready || missing {
			if missing {
				err = installPrepared(target)
			}
			if err != nil {
				lock.Close()
				return err
			}
			if mode == "sessions" {
				lock.Close()
				return listSessions()
			}
			readyMode := "managed"
			if updated {
				readyMode = "updated"
			}
			if err = writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: readyMode}); err != nil {
				lock.Close()
				return err
			}
			return bridgePrepared(name, mode == "resume", target, lock)
		}
		lock.Close()
		if mode == "sessions" {
			return fmt.Errorf("remote version mismatch; connect interactively to update or recover")
		}
		action := "update"
		if state.Manual {
			action = "manual-only"
		}
		installed := state.Installed
		if installed == "" {
			installed = "not installed"
		}
		detail := fmt.Sprintf("Remote: %s. Required: %s.", installed, version)
		if changed {
			detail = "Remote state changed; review again. " + detail
		}
		if state.Manual {
			detail += " An installed program or session has an unknown version; automatic update is unavailable."
		}
		if err = writeMessage(os.Stdout, message{Type: "setup_choice", SoftwareVersion: version, Action: action, Error: detail, SetupProcesses: state.Processes}); err != nil {
			return err
		}
		reply, err := readMessage(os.Stdin)
		if err != nil {
			return err
		}
		if reply.Type != "setup_action" {
			return fmt.Errorf("invalid setup action")
		}
		if reply.Action == "plain" {
			if err = writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: "plain"}); err != nil {
				return err
			}
			return recoveryShell()
		}
		if reply.Action != "update" || state.Manual {
			return fmt.Errorf("automatic update is not authorized")
		}
		lock, err = setupLock()
		if err != nil {
			return err
		}
		current, err := inspectSetup(target)
		if err != nil {
			lock.Close()
			return err
		}
		if !reflect.DeepEqual(state, current) {
			lock.Close()
			changed = true
			continue
		} // require fresh confirmation
		// Stage on the target filesystem and verify before stopping any process.
		staged, err := prepareReplacement(target)
		if err != nil {
			lock.Close()
			return offerRecovery(err)
		}
		err = stopOldSessions(state.Processes)
		if err == nil {
			err = os.Rename(staged, target)
		}
		_ = os.Remove(staged)
		lock.Close()
		if err != nil {
			return offerRecovery(err)
		}
		updated = true
		if mode == "resume" {
			mode = "bridge"
		}
		// Recheck the complete state before launching a session.
	}
}

func prepareReplacement(target string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return "", err
	}

	dirInfo, err := os.Stat(filepath.Dir(target))
	if err != nil {
		return "", err
	}
	owner, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || dirInfo.Mode().Perm()&0022 != 0 {
		return "", fmt.Errorf("automatic update requires a user-owned directory without group/world write access; install manually")
	}
	if info, e := os.Lstat(target); e == nil {
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() {
			return "", fmt.Errorf("automatic update requires a user-owned regular executable; install manually")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return "", e
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	source, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer source.Close()
	file, err := os.CreateTemp(filepath.Dir(target), ".sshore-update-*")
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
	v, err := executableVersion(path)
	if err != nil || v != version {
		return "", fmt.Errorf("prepared executable verification failed: %v", err)
	}
	// Probe atomic replacement permissions before terminating sessions.
	if info, e := os.Lstat(target); e == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("automatic update requires a regular executable target")
	}
	if old, e := executableVersion(target); e == nil && old != version {
		if _, ok := releaseSeries(old); ok {
			if err := os.Link(target, target+"-"+old); err != nil && !errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("back up old executable: %w", err)
			}
		}
	}
	success = true
	return path, nil
}

func installPrepared(target string) error {
	staged, err := prepareReplacement(target)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	return os.Rename(staged, target)
}

func recoveryShell() error {
	transport := &pipeTransport{Reader: os.Stdin, Writer: os.Stdout}
	m, err := readMessage(transport)
	if err != nil {
		return err
	}
	if m.Type != "mux" || m.SoftwareVersion != version {
		return fmt.Errorf("invalid recovery handshake")
	}
	if err = writeMessage(transport, message{Type: "mux_ready", SoftwareVersion: version}); err != nil {
		return err
	}
	s := &session{plain: true, done: make(chan struct{})}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.master != nil {
			_ = s.master.Close()
		}
	}()
	return s.serveMux(transport)
}

// pidfds bind signals to the inspected process, never a reused numeric PID.
func signalProcess(pid int, start string, signal unix.Signal) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	current, err := processStart(pid)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != start {
		return fmt.Errorf("process %d changed during setup", pid)
	}
	if err = unix.PidfdSendSignal(fd, signal, nil, 0); errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func offerRecovery(cause error) error {
	if err := writeMessage(os.Stdout, message{Type: "setup_choice", SoftwareVersion: version, Action: "manual-only", Error: "Setup could not complete: " + cause.Error()}); err != nil {
		return err
	}
	reply, err := readMessage(os.Stdin)
	if err != nil {
		return err
	}
	if reply.Type != "setup_action" || reply.Action != "plain" {
		return fmt.Errorf("invalid recovery choice")
	}
	if err := writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: "plain"}); err != nil {
		return err
	}
	return recoveryShell()
}

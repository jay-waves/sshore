package main

// Setup prepares the exact release before requesting reviewed shutdown of
// the account daemon. It rechecks the confirmed task snapshot under the setup
// lock; unavailable control, changed state or refusal never triggers process scans
// or client-side forced cleanup. Failures stop setup without a recovery shell.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"

	"golang.org/x/sys/unix"
)

func bootstrap(target, mode, name string) (result error) {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("setup target must be absolute")
	}
	if mode != "bridge" && mode != "resume" && mode != "sessions" && mode != "new" && mode != "attach" && mode != "spawn" && mode != "mount-agent" && mode != "cp-agent" && mode != "mcp-agent" {
		return fmt.Errorf("invalid setup mode")
	}
	if mode == "resume" || name != "" {
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
		state, err := inspectSetup(target, false)
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
			if mode == "attach" && name == "" {
				names, err := detachedSessionNames()
				lock.Close()
				if err != nil {
					return err
				}
				if len(names) == 0 {
					return fmt.Errorf("no detached shells; use sshore launch HOST to create one")
				}
				if len(names) == 1 {
					name = names[0]
					continue
				}
				if err := writeMessage(os.Stdout, message{Type: "session_choice", SoftwareVersion: version, Names: names}); err != nil {
					return err
				}
				reply, err := readMessage(os.Stdin)
				if err != nil {
					return err
				}
				if reply.Type != "session_select" {
					return fmt.Errorf("invalid shell selection")
				}
				for _, candidate := range names {
					if reply.Name == candidate {
						name = candidate
						break
					}
				}
				if name == "" {
					return fmt.Errorf("invalid shell selection")
				}
				continue // Recheck setup after waiting without the account lock.
			}
			readyMode := "managed"
			if updated {
				readyMode = "updated"
			}
			if mode == "mcp-agent" {
				c, err := connectRemoteDaemon(target)
				lock.Close()
				if err != nil {
					return err
				}
				defer c.Close()
				if err := writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: readyMode}); err != nil {
					return err
				}
				return bridgeMCP(c, target)
			}
			if mode == "mount-agent" || mode == "cp-agent" {
				lock.Close()
				if err := writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: readyMode}); err != nil {
					return err
				}
				return serverCommand([]string{mode})
			}
			return bridgePrepared(name, mode == "resume" || mode == "attach", mode == "new" || mode == "spawn", target, lock, readyMode, mode == "spawn")
		}
		if mode == "sessions" {
			lock.Close()
			return fmt.Errorf("remote version mismatch; connect interactively to update")
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
		action := "update"
		installed := state.Installed
		if installed == "" {
			installed = "not installed"
		}
		detail := fmt.Sprintf("Remote: %s. Required: %s.", installed, version)
		if changed {
			detail = "Remote state changed; review again. " + detail
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
		if reply.Action == "cancel" || reply.Action == "plain" {
			return fmt.Errorf("setup cancelled; no tasks ended")
		}
		if reply.Action != "update" || state.Manual {
			return fmt.Errorf("automatic update is not authorized")
		}
		lock, err = setupLock()
		if err != nil {
			return err
		}
		current, err := inspectSetup(target, false)
		if err != nil {
			lock.Close()
			return err
		}
		if !reflect.DeepEqual(state, current) {
			lock.Close()
			changed = true
			continue
		} // require fresh confirmation
		// The staged replacement must still be verified before ending tasks.
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
		_ = os.Remove(staged)
		lock.Close()
		if err != nil {
			return err
		}
		updated = true
		if mode == "resume" {
			return writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: "updated"})
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
	path, err := stageReplacement(target)
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			os.Remove(path)
		}
	}()
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

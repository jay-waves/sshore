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
	"syscall"
)

func bootstrap(target, mode, name string) (result error) {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("setup target must be absolute")
	}
	if mode != "prepare" && mode != "bridge" && mode != "resume" && mode != "sessions" && mode != "new" && mode != "attach" && mode != "spawn" && mode != "mount-agent" && mode != "cp-agent" && mode != "mcp-agent" {
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
		if state.Installed != version || state.Daemon != nil || state.Manual {
			lock.Close()
			if err := installPreparedRelease(target, true, remoteSetupConfirmation); err != nil {
				return err
			}
			continue
		}
		if mode == "prepare" {
			lock.Close()
			return nil
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
		return bridgePrepared(name, mode, target, lock, readyMode)
	}
}

func remoteSetupConfirmation(m message) (string, error) {
	m.Type, m.SoftwareVersion = "setup_choice", version
	if machine := systemMachineID(); machine != "" {
		m.SourceMachineID = taskUID(fmt.Sprintf("%s/%d", machine, os.Geteuid()))
	} else {
		id, err := newTaskUID()
		if err != nil {
			return "", err
		}
		m.SourceMachineID = id
	}
	if err := writeMessage(os.Stdout, m); err != nil {
		return "", err
	}
	reply, err := readMessage(os.Stdin)
	if err != nil {
		return "", err
	}
	if reply.Type != "setup_action" {
		return "", fmt.Errorf("invalid setup action")
	}
	return reply.Action, nil
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

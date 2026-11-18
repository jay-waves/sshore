package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func serveRemote(mode, name, peerVersion string) (result error) {
	target, err := os.Executable()
	if err != nil {
		return err
	}
	if mode != "bridge" && mode != "resume" && mode != "new" && mode != "attach" && mode != "spawn" && mode != "mount-agent" && mode != "cp-agent" && mode != "mcp-agent" {
		return fmt.Errorf("invalid remote service")
	}
	if mode == "resume" || name != "" {
		if err := validSession(name); err != nil {
			return err
		}
	}
	// Fatal setup replies are framed; no human text ever enters protocol stdout.
	defer func() {
		if result != nil {
			_ = writeMessage(os.Stdout, message{Type: "fatal", SoftwareVersion: version, Error: result.Error()})
		}
	}()
	if err := checkPeerVersion(peerVersion, "client"); err != nil {
		return err
	}
	for {
		lock, err := setupLock()
		if err != nil {
			return err
		}
		if err := requireInstalledRelease(target); err != nil {
			lock.Close()
			return err
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
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := ensureLocalDaemonContext(ctx, target)
			cancel()
			lock.Close()
			if err != nil {
				return err
			}
			if err := writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: readyMode}); err != nil {
				return err
			}
			return bridgeMCP(target)
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
	// Probe atomic replacement permissions before publishing.
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

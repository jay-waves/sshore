package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func serveRemote(mode, name, peerVersion string) (result error) {
	target, err := os.Executable()
	if err != nil {
		return err
	}
	if mode != "bridge" && mode != "resume" && mode != "new" && mode != "attach" && mode != "spawn" && mode != "mount-agent" && mode != "cp-agent" {
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
			lock.Close()
			selected, err := chooseDetachedHostedShell()
			if err != nil {
				return err
			}
			name = selected.Instance
			continue // Recheck setup after waiting without the account lock.
		}
		readyMode := "managed"
		if mode == "mount-agent" || mode == "cp-agent" {
			lock.Close()
			home, _ := os.UserHomeDir()
			if err := writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: readyMode, HomeDirectory: home}); err != nil {
				return err
			}
			return serverCommand([]string{mode})
		}
		return bridgePrepared(name, mode, target, lock, readyMode)
	}
}

func chooseDetachedHostedShell() (sessionTarget, error) {
	targets, err := detachedHostedShells()
	if err != nil {
		return sessionTarget{}, err
	}
	switch len(targets) {
	case 0:
		return sessionTarget{}, fmt.Errorf("no detached shells; use sshore launch HOST: to create one")
	case 1:
		return targets[0], nil
	}
	if err := writeMessage(os.Stdout, message{Type: "session_choice", SoftwareVersion: version, Targets: targets}); err != nil {
		return sessionTarget{}, err
	}
	reply, err := readMessage(os.Stdin)
	if err != nil {
		return sessionTarget{}, err
	}
	if reply.Type == "session_select" {
		for _, shell := range targets {
			if reply.Name == shell.Name {
				return shell, nil
			}
		}
	}
	return sessionTarget{}, fmt.Errorf("invalid shell selection")
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

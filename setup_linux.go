package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func serveRemote(req remoteServiceRequest) (result error) {
	mode, name := req.Mode, req.Name
	fileService := req.Operation != "shell"
	target, err := os.Executable()
	if err != nil {
		return err
	}
	if !fileService && mode != "bridge" && mode != "new" && mode != "attach" && mode != "spawn" {
		return fmt.Errorf("invalid remote service")
	}
	if name != "" {
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
	if err := checkPeerVersion(req.Release, "client"); err != nil {
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
		if !fileService && mode == "attach" && name == "" {
			lock.Close()
			selected, err := chooseDetachedHostedShell()
			if err != nil {
				return err
			}
			name = selected.Instance
			continue // Recheck setup after waiting without the account lock.
		}
		if fileService {
			lock.Close()
			home, _ := os.UserHomeDir()
			if err := writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", HomeDirectory: home}); err != nil {
				return err
			}
			return serveRemoteFiles(req.Operation)
		}
		return prepareHostedShell(name, mode, target, lock)
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

func runLocalInstaller(target string) error {
	if target == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		target = filepath.Join(filepath.Dir(executable), "sshore")
	}
	cmd := exec.Command("sh", "-c", linuxInstaller, "install.sh", "--path", target)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

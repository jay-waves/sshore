package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func serveRemote(req remoteServiceRequest) (result error) {
	mode, name := req.Mode, req.Name
	fileService := req.Operation != "shell"
	target, err := os.Executable()
	if err != nil {
		return err
	}
	if !fileService && mode != "bridge" && mode != "new" && mode != "attach" {
		return fmt.Errorf("invalid remote service")
	}
	if mode == "attach" && name == "" {
		name = req.Instance
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
		return prepareHostedShell(name, mode, req.Instance, target, lock)
	}
}

func chooseDetachedHostedShell() (sessionTarget, error) {
	rows, err := queryRegisteredTasks(taskQuery{Kind: "hosted", Detached: true})
	if err != nil {
		return sessionTarget{}, err
	}
	targets, err := attachmentChoices(rows)
	if err != nil {
		return sessionTarget{}, err
	}
	switch len(targets) {
	case 0:
		return sessionTarget{}, fmt.Errorf("no detached shells; use sshore sh HOST: to create one")
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

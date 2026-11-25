package main

import (
	"os"
	"path/filepath"

	"github.com/jay-waves/sshore/internal/files"
)

func serveRemoteRequest(req remoteServiceRequest) error {
	if req.Operation == "shell" {
		return serveRemote(req, nil)
	}
	if req.Operation == "task-control" {
		if err := checkPeerVersion(req.Release, "client"); err != nil {
			return err
		}
		return serveTaskControl(req)
	}
	if req.Operation != "mount-agent" {
		return usage("unknown remote service operation")
	}

	account, stop, err := prepareWindowsMountAgent(req)
	if err != nil {
		_ = writeMessage(os.Stdout, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		return err
	}
	defer stop()
	home, _ := os.UserHomeDir()
	if filepath.IsAbs(home) {
		home = "/" + filepath.ToSlash(home)
	}
	if err := writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", HomeDirectory: home}); err != nil {
		return err
	}
	// Windows mount agents accept direct mounts. Shell approvals require a route validator.
	return files.ServeAgentWithPeer(agentStdio(), version, account, beginIncomingMountTask)
}

func prepareWindowsMountAgent(req remoteServiceRequest) (string, func(), error) {
	if err := checkPeerVersion(req.Release, "client"); err != nil {
		return "", nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	lock, err := setupLock()
	if err != nil {
		return "", nil, err
	}
	defer lock.Close()
	if err := requireInstalledRelease(executable); err != nil {
		return "", nil, err
	}
	stop, err := beginCommandActivity()
	if err != nil {
		return "", nil, err
	}
	account, _ := localMachineIdentity()
	return account, stop, nil
}

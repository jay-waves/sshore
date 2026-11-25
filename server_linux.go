//go:build linux

package main

import (
	"os"

	"github.com/jay-waves/sshore/internal/files"
)

func serveRemoteRequest(req remoteServiceRequest) error {
	switch req.Operation {
	case "shell", "cp-agent", "mount-agent":
		return serveRemote(req, serveRemoteFiles)
	case "forward-agent":
		return serveForwardAgent(req)
	case "daemon-admin":
		// Maintenance keeps its own snapshot and shutdown verification.
		return serveDaemonAdmin(os.Stdin, os.Stdout)
	}
	if err := checkPeerVersion(req.Release, "client"); err != nil {
		return err
	}
	if req.Operation == "task-control" {
		return serveTaskControl(req)
	}
	return usage("unknown remote service operation")
}

func serveRemoteFiles(operation string) error {
	stop, err := leaseActivity(leaseRequest{Kind: "heartbeat"})
	if err != nil {
		return err
	}
	defer stop()
	if operation == "cp-agent" {
		return files.ServeCopy(agentStdio())
	}
	account, _ := localMachineIdentity()
	return files.ServeAgentWithPeer(agentStdio(), version, account, beginIncomingMountTask)
}

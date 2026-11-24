//go:build linux

package main

import (
	"fmt"
	"os"

	"github.com/jay-waves/sshore/internal/files"
)

func serveRemoteRequest(req remoteServiceRequest) error {
	switch req.Operation {
	case "shell", "cp-agent", "mount-agent":
		return serveRemote(req)
	case "daemon-admin":
		// Maintenance keeps its own snapshot and shutdown verification.
		return serveDaemonAdmin(os.Stdin, os.Stdout)
	}
	if err := checkPeerVersion(req.Release, "client"); err != nil {
		return err
	}
	switch req.Operation {
	case "forward-agent":
		if req.Forward == nil || req.Forward.Kind != "forward" {
			return fmt.Errorf("invalid forward request")
		}
		f := req.Forward
		if err := (forwardConfig{Port: f.Port, Target: f.TargetPort}).Validate(); err != nil {
			return err
		}
		serveRoutedForward(agentStdio(), *f)
		return nil
	case "task-control":
		return serveTaskControl(req)
	}
	return usage("unknown remote service operation")
}

func serveTaskControl(req remoteServiceRequest) error {
	if req.Mode != "kill" && req.Mode != "rename" {
		return usage("invalid task operation")
	}
	if err := validSession(req.Name); err != nil {
		return err
	}
	target, err := selectRegisteredTask(taskQuery{Ref: req.Name, UID: req.Instance})
	if err != nil {
		return err
	}
	_, err = mutateRegisteredTask(req.Mode, target, req.NewName)
	return err
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

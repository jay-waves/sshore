//go:build linux

package main

import "os"

func serveRemoteRequest(req remoteServiceRequest) error {
	switch req.Operation {
	case "shell":
		return serveRemote(req)
	case "cp-agent", "mount-agent":
		return serveRemoteFileService(req, func() (func(), error) {
			return leaseActivity(leaseRequest{Kind: "heartbeat"})
		})
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

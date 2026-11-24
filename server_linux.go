//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
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
	case "forward-watch", "task-watch":
		for _, name := range []string{"SSHORE_CONFIRM_ADDR", "SSHORE_CONFIRM_TOKEN", "SSHORE_SOCKET"} {
			_ = os.Unsetenv(name)
		}
		if req.Operation == "forward-watch" {
			if err := checkApprovalSource(req.Name, req.PID, req.ApprovalID, req.Instance); err != nil {
				return err
			}
		}

		if _, err := fmt.Fprintln(os.Stdout, forwardReadyMarker); err != nil {
			return err
		}
		return watchRemoteTask()
	case "task-control":
		return serveTaskControl(req)
	case "console-inventory":
		return writeMessage(os.Stdout, consoleInventory(message{Name: req.Mode}, ""))
	case "inventory":
		return json.NewEncoder(os.Stdout).Encode(localSessionInventory())
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
	_, err = withDaemonActivity(func() (int, error) { return mutateRegisteredTask(req.Mode, target, req.NewName) })
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
	return files.ServeAgentWithPeer(agentStdio(), version, account, beginIncomingMountTask, func(a files.MountApproval) error {
		return checkApprovalSource(a.Session, a.PID, a.ID, a.Instance)
	})
}

// The existing SSH transport owns this watcher. EOF or termination removes its
// lease, removing its resource from the incoming inventory.
func watchRemoteTask() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	// Inherited stdin can block even after a signal. Give the watcher a reader
	// we can close independently so it releases the lease before this helper exits.
	input, output := io.Pipe()
	defer input.Close()
	stop := context.AfterFunc(ctx, func() { input.Close() })
	defer stop()
	go func() {
		_, err := io.Copy(output, os.Stdin)
		_ = output.CloseWithError(err)
	}()
	err := watchPeerRecords(input, "", func(err error) {
		termui.Log(os.Stderr, "", termui.Warning, "[sshore] Peer inventory unavailable; forward active: %s\n", cleanText(err.Error()))
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

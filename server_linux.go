//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
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
	case "forward-watch":
		if err := checkApprovalSource(req.Name, req.PID, req.ApprovalID, req.Instance); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(os.Stdout, forwardReadyMarker); err != nil {
			return err
		}
		return watchRemoteTask()
	case "rename-session":
		return renameSession(req.Name, req.NewName, req.Instance)
	case "kill-session":
		return killSession(req.Name, req.Instance)
	case "task-watch":
		return watchRemoteTask()
	case "task-remove":
		return removeRemoteTask(req.Identity)
	case "inventory":
		return json.NewEncoder(os.Stdout).Encode(localSessionInventory())
	}
	return usage("unknown remote service operation")
}

func serveRemoteFiles(operation string) error {
	stop, err := leaseActivity(activityRequest{Kind: "heartbeat"})
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
// one lease; a crash leaves a record that expires to unknown, never a live claim.
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
		termui.Log(os.Stderr, termui.Warning, "[sshore] Peer inventory unavailable; forward active: %s\n", cleanText(err.Error()))
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (s *remoteShell) serveSpawnOwner(c net.Conn, first message) {
	s.mu.Lock()
	if s.lifetime != awaitingSpawnOwner || checkPeerVersion(first.SoftwareVersion, "client") != nil || first.Instance != s.record.UID {
		s.mu.Unlock()
		_ = writeMessage(c, message{Type: "error", Error: "spawn lifetime already owned or unavailable"})
		return
	}
	s.lifetime = ownedSpawn
	instance := s.record.UID
	s.mu.Unlock()
	defer func() { _ = s.kill(instance, "spawn lifetime connection closed") }()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-s.done:
			c.Close()
		case <-done:
		}
	}()
	if activity := accountActivity.Load(); activity != nil {
		activity.serve(c, activityRequest{Kind: "heartbeat"})
	}
}

type stdioPipe struct {
	io.Reader
	io.Writer
	closers []io.Closer
	once    sync.Once
}

func (p *stdioPipe) Close() error {
	p.once.Do(func() {
		for _, c := range p.closers {
			_ = c.Close()
		}
	})
	return nil
}

func agentStdio() *stdioPipe {
	return &stdioPipe{Reader: os.Stdin, Writer: os.Stdout, closers: []io.Closer{os.Stdin, os.Stdout}}
}

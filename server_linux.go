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

func serverCommand(args []string) error {
	switch args[0] {
	case "forward-watch":
		return runApprovedForwardWatch(args[1:])
	case "mcp-agent":
		if len(args) != 2 {
			return usage("mcp-agent requires a client version")
		}
		return serveRemote(args[0], "", args[1])
	case "cp-agent":
		if len(args) == 2 {
			return serveRemote(args[0], "", args[1])
		}
		if len(args) != 1 {
			return usage("cp-agent takes no arguments")
		}
		stop, err := leaseActivity(activityRequest{Kind: "heartbeat"})
		if err != nil {
			return err
		}
		defer stop()
		return files.ServeCopy(agentStdio())
	case "mount-agent":
		if len(args) == 2 {
			return serveRemote(args[0], "", args[1])
		}
		if len(args) != 1 {
			return usage("mount-agent takes no arguments")
		}
		stop, err := leaseActivity(activityRequest{Kind: "heartbeat"})
		if err != nil {
			return err
		}
		defer stop()
		account, _ := localMachineIdentity()
		return files.ServeAgentWithPeer(agentStdio(), version, account, beginIncomingMountTask, func(a files.MountApproval) error { return checkApprovalSource(a.Session, a.PID, a.ID, a.Instance) })
	case "rename-session":
		if len(args) != 4 && len(args) != 5 {
			return usage("rename-session requires old name, new name and client version")
		}
		if err := checkPeerVersion(args[3], "client"); err != nil {
			return err
		}
		return renameSession(args[1], args[2], args[4:]...)
	case "kill-session":
		if len(args) != 3 && len(args) != 4 {
			return usage("kill-session requires a name and client version")
		}
		if err := checkPeerVersion(args[2], "client"); err != nil {
			return err
		}
		return killSession(args[1], args[3:]...)
	case "bridge", "resume":
		if args[0] == "bridge" && len(args) == 4 {
			return serveRemote(args[2], args[3], args[1])
		}
		if len(args) != 2 {
			return fmt.Errorf("internal command requires a shell name")
		}
		return bridge(args[1], args[0])
	case "task-watch":
		if len(args) != 2 {
			return usage("invalid task watcher arguments")
		}
		if e := checkPeerVersion(args[1], "client"); e != nil {
			return e
		}
		return watchRemoteTask()
	case "task-remove":
		if len(args) != 3 {
			return usage("invalid task removal arguments")
		}
		if e := checkPeerVersion(args[1], "client"); e != nil {
			return e
		}
		return removeRemoteTask(args[2])
	case "inventory":
		if len(args) != 2 && (len(args) != 3 || args[2] != "--diagnostics") {
			return usage("inventory requires client version")
		}
		if e := checkPeerVersion(args[1], "client"); e != nil {
			return e
		}
		inv := localSessionInventory(len(args) == 3)
		return json.NewEncoder(os.Stdout).Encode(inv)
	case "sessions":
		if len(args) != 1 {
			return usage("usage: sshore sessions")
		}
		return listSessions()
	case "detach":
		if len(args) != 1 {
			return usage("usage: sshore detach")
		}
		return detachHelper()

	}
	return fmt.Errorf("unknown server command")
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
	if s.lifetime != awaitingSpawnOwner || checkPeerVersion(first.SoftwareVersion, "client") != nil || first.Instance != s.instance {
		s.mu.Unlock()
		_ = writeMessage(c, message{Type: "error", Error: "spawn lifetime already owned or unavailable"})
		return
	}
	s.lifetime = ownedSpawn
	instance := s.instance
	s.mu.Unlock()
	defer func() { _ = s.killWithReason(instance, "spawn lifetime connection closed") }()

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

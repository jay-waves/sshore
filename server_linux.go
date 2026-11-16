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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/xtaci/smux"
)

func serverCommand(args []string) error {
	switch args[0] {
	case "forward-watch":
		return runApprovedForwardWatch(args[1:])
	case "cp-agent":
		if len(args) != 1 {
			return usage("cp-agent takes no arguments")
		}
		return files.ServeCopy(&transportio.Pipe{Reader: os.Stdin, Writer: os.Stdout, Closers: []io.Closer{os.Stdin, os.Stdout}})
	case "mount-agent":
		if len(args) != 1 {
			return usage("mount-agent takes no arguments")
		}
		return files.ServeAgentWithPeer(&transportio.Pipe{Reader: os.Stdin, Writer: os.Stdout, Closers: []io.Closer{os.Stdin, os.Stdout}}, version, func(data json.RawMessage) func() {
			var task peerRecord
			if len(data) > maxRemoteTaskSize || json.Unmarshal(data, &task) != nil || !strings.HasPrefix(task.ID, "mount/") {
				return func() {}
			}
			return beginIncomingTask(task)
		}, func(a files.MountApproval) error { return checkApprovalSource(a.Session, a.PID, a.ID) })
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
	case "bootstrap":
		if len(args) != 4 {
			return usage("invalid bootstrap arguments")
		}
		return bootstrap(args[1], args[2], args[3])
	case "bridge", "resume", "daemon":
		if args[0] == "daemon" && len(args) == 1 {
			return runSharedRemoteDaemon()
		}
		if len(args) != 2 {
			return fmt.Errorf("internal command requires a shell name")
		}
		if args[0] == "bridge" || args[0] == "resume" {
			return bridge(args[1], args[0] == "resume")
		}
		return daemon(args[1])
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
		inv, e := localSessionInventory(len(args) == 3)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(inv)
	case "sessions":
		if len(args) != 1 {
			return usage("usage: sshore sessions")
		}
		return listSessions()
	case "mount":
		op, err := remoteMountRequest(args[1:])
		if err != nil {
			return err
		}
		op.Path, err = filepath.Abs(op.Path)
		if err != nil {
			return err
		}
		info, err := os.Stat(op.Path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("mount source must be a directory")
		}
		return requestOperations([]operation{op})
	case "forward":
		op, err := remoteForwardRequest(args[1:])
		if err != nil {
			return err
		}
		return requestOperations([]operation{op})
	case "detach":
		if len(args) != 1 {
			return usage("usage: sshore detach")
		}
		return detachHelper()
	case "cp":
		kind, paths, hint, err := remoteCopyRequest(args[1:])
		if err != nil {
			return err
		}
		return requestFilesWithHint(kind, paths, hint)
	}
	return fmt.Errorf("unknown server command")
}

// The existing SSH transport owns this watcher. EOF or termination removes its
// one lease; a crash leaves a record that expires to unknown, never a live claim.
func watchRemoteTask() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	return watchPeerRecords(ctx, os.Stdin, "", func(err error) {
		termui.Log(os.Stderr, termui.Warning, "[sshore] Peer inventory unavailable; forward active: %s\n", cleanText(err.Error()))
	})
}

// The original SSH connection is the lifetime lease; individual attachments
// can end without closing it. Its first stream carries the initial attachment.
func bridgeSpawn(path string, lease net.Conn, readyMode string) error {
	return bridgeSpawnTransport(path, lease, readyMode, &transportio.Pipe{Reader: os.Stdin, Writer: os.Stdout})
}

func bridgeSpawnTransport(path string, lease net.Conn, readyMode string, transport io.ReadWriteCloser) error {
	if err := writeMessage(lease, message{Type: "spawn_owner", SoftwareVersion: version}); err != nil {
		return err
	}
	reply, err := readMessage(lease)
	if err != nil {
		return err
	}
	if reply.Type != "spawn_owned" {
		return fmt.Errorf("spawn lease rejected: %s", reply.Error)
	}
	if readyMode != "" {
		if err = writeMessage(transport, message{Type: "setup_ready", SoftwareVersion: version, Action: readyMode}); err != nil {
			return err
		}
	}
	first, err := readMessage(transport)
	if err != nil {
		return err
	}
	if first.Type != "mux" || checkPeerVersion(first.SoftwareVersion, "client") != nil {
		return fmt.Errorf("invalid spawn transport")
	}
	if err = writeMessage(transport, message{Type: "mux_ready", SoftwareVersion: version}); err != nil {
		return err
	}
	mx, err := smux.Server(transport, transportio.MuxConfig())
	if err != nil {
		return err
	}
	defer mx.Close()
	go func() { _, _ = io.Copy(io.Discard, lease); mx.Close() }()
	slots := make(chan struct{}, 8)
	var handlers sync.WaitGroup
	defer handlers.Wait()
	for {
		stream, err := mx.AcceptStream()
		if err != nil {
			return nil
		}
		select {
		case slots <- struct{}{}:
		default:
			stream.Close()
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-slots }()
			defer stream.Close()
			c, err := net.DialTimeout("unix", path, time.Second)
			if err != nil {
				return
			}
			defer c.Close()
			proxySpawn(stream, c)
		}()
	}
}

func (s *remoteShell) serveSpawnOwner(c net.Conn, first message) {
	s.mu.Lock()
	if !s.temporary || s.spawnOwned || checkPeerVersion(first.SoftwareVersion, "client") != nil {
		s.mu.Unlock()
		_ = writeMessage(c, message{Type: "error", Error: "spawn lifetime already owned or unavailable"})
		return
	}
	s.spawnOwned = true
	instance := s.instance
	s.mu.Unlock()
	defer func() { _ = s.cancel(instance) }()
	if writeMessage(c, message{Type: "spawn_owned"}) != nil {
		return
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-s.done:
			c.Close()
		case <-done:
		}
	}()
	_, _ = io.Copy(io.Discard, c)
}

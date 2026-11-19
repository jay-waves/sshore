//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/wire"
	"github.com/xtaci/smux"
)

func (s *remoteShell) serveRoutedResource(c net.Conn, request message) {
	fail := func(err error) {
		_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
	}
	if err := checkPeerVersion(request.SoftwareVersion, "resource client"); err != nil {
		fail(err)
		return
	}
	if request.Instance == "" || os.Getenv("SSHORE_DISABLED") == "1" {
		fail(fmt.Errorf("resource shell changed or unavailable"))
		return
	}
	var forward operation
	switch request.Type {
	case "route_forward":
		if len(request.Operations) != 1 || request.Operations[0].Kind != "forward" {
			fail(fmt.Errorf("invalid forward request"))
			return
		}
		forward = request.Operations[0]
		if err := (forwardConfig{Port: forward.Port, Target: forward.TargetPort}).Validate(); err != nil {
			fail(err)
			return
		}
	case "route_mount", "route_files":
	default:
		fail(fmt.Errorf("invalid resource request"))
		return
	}
	if err := s.checkApprovalSource(request); err != nil {
		fail(err)
		return
	}
	if request.Type == "route_forward" {
		serveRoutedForward(c, forward)
		return
	}
	if err := writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}); err != nil {
		return
	}
	if request.Type == "route_mount" {
		account, _ := localMachineIdentity()
		_ = files.ServeAgentWithPeer(c, version, account, func(data json.RawMessage) func() {
			var task peerRecord
			if len(data) > maxRemoteTaskSize || json.Unmarshal(data, &task) != nil || !strings.HasPrefix(task.ID, "mount/") {
				return func() {}
			}
			return beginIncomingTask(task)
		}, func(a files.MountApproval) error {
			if a.Session != request.Name || a.PID != request.PID || a.ID != request.ApprovalID || a.Instance != "" && a.Instance != request.Instance {
				return fmt.Errorf("mount approval identity changed")
			}
			return s.checkApprovalSource(request)
		})
		return
	}
	mx, err := smux.Server(c, wire.MuxConfig())
	if err != nil {
		return
	}
	defer mx.Close()
	serveApprovedStreams(context.Background(), mx)
}

func serveRoutedForward(c net.Conn, forward operation) {
	var listener net.Listener
	if forward.Reverse {
		var err error
		listener, err = net.Listen("tcp", loopbackPort(forward.Port))
		if err != nil {
			_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
			return
		}
		defer func() {
			if listener != nil {
				_ = listener.Close()
			}
		}()
	}
	if err := writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}); err != nil {
		return
	}
	tunnel, control, err := openResourceMuxForward(c, listener, forward.TargetPort, false)
	if err != nil {
		return
	}
	listener = nil // The forwarding workers now own the listener.
	defer tunnel.Close()
	if _, err = fmt.Fprintln(control, forwardReadyMarker); err != nil {
		return
	}
	_ = watchPeerRecords(control, "forward", nil)
}

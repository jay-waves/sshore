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
	s.mu.Lock()
	unavailable := s.lifecycle.Exited() || request.Instance == "" || request.Instance != s.instance
	s.mu.Unlock()
	if unavailable || os.Getenv("SSHORE_DISABLED") == "1" {
		fail(fmt.Errorf("resource shell changed or unavailable"))
		return
	}
	if request.Type == "route_forward" {
		if len(request.Operations) != 1 || request.Operations[0].Kind != "forward" {
			fail(fmt.Errorf("invalid forward request"))
			return
		}
		op := request.Operations[0]
		if err := (forwardConfig{Port: op.Port, Target: op.TargetPort}).Validate(); err != nil {
			fail(err)
			return
		}
	} else if request.Type != "route_mount" && request.Type != "route_files" {
		fail(fmt.Errorf("invalid resource request"))
		return
	}
	if err := s.checkApprovalSource(request); err != nil {
		fail(err)
		return
	}
	if request.Type == "route_files" {
		if err := writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}); err != nil {
			return
		}
		mx, err := smux.Server(c, wire.MuxConfig())
		if err != nil {
			return
		}
		defer mx.Close()
		serveApprovedStreams(context.Background(), mx)
		return
	}
	if request.Type == "route_mount" {
		if err := writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}); err != nil {
			return
		}
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
	op := request.Operations[0]
	var listener net.Listener
	if op.Reverse {
		var err error
		listener, err = net.Listen("tcp", loopbackPort(op.Port))
		if err != nil {
			fail(err)
			return
		}
		defer listener.Close()
	}
	if err := writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}); err != nil {
		return
	}
	mx, err := smux.Server(c, wire.MuxConfig())
	if err != nil {
		return
	}
	defer mx.Close()
	control, err := mx.AcceptStream()
	if err != nil {
		return
	}
	defer control.Close()
	forward := wire.StartMuxForward(mx, listener, op.TargetPort)
	defer forward.Close()
	if _, err = fmt.Fprintln(control, forwardReadyMarker); err != nil {
		return
	}
	_ = watchPeerRecords(context.Background(), control, "forward", nil)
}

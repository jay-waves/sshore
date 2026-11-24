//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	forwardio "github.com/jay-waves/sshore/internal/forward"
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
	serveApprovedResource(c, request, s.checkApprovalSource)
}

// Authorization stays with the shell route; resource endpoints own their I/O.
func serveApprovedResource(c net.Conn, request message, check func(message) error) {
	if request.Type == "route_forward" {
		forward := request.Operations[0]
		serveRoutedForward(c, forward)
		return
	}
	if err := writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}); err != nil {
		return
	}
	if request.Type == "route_mount" {
		account, _ := localMachineIdentity()
		_ = files.ServeAgentWithPeer(c, version, account, beginIncomingMountTask, func(a files.MountApproval) error {
			if a.Session != request.Name || a.PID != request.PID || a.ID != request.ApprovalID || a.Instance != "" && a.Instance != request.Instance {
				return fmt.Errorf("mount approval identity changed")
			}
			return check(request)
		})
		return
	}
	serveApprovedFileEndpoint(c)
}

func serveApprovedFileEndpoint(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(requestTimeout))
	m, err := readMessage(c)
	if err != nil {
		return
	}
	if m.Type != "approved_transfer" {
		_ = writeMessage(c, message{Type: "error", Error: "invalid transfer request"})
		return
	}
	approvedRemoteTransfer(c, m)
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
	tunnel, control, err := forwardio.Open(c, listener, forward.TargetPort, false)
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

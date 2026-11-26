//go:build linux || windows

package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/jay-waves/sshore/internal/files"
)

type resourceApproval struct {
	id         string
	operations []operation
}

func (s *remoteShell) serveRoutedResource(c net.Conn, request message, relay func(net.Conn, message) error) {
	fail := func(err error) {
		_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
	}
	if err := checkPeerVersion(request.SoftwareVersion, "resource client"); err != nil {
		fail(err)
		return
	}
	if !validTaskUID(request.Instance) || os.Getenv("SSHORE_DISABLED") == "1" {
		fail(fmt.Errorf("resource shell changed or unavailable"))
		return
	}
	if request.Type == "route_files" {
		if len(request.ResourceRoute) != 0 {
			fail(fmt.Errorf("invalid file route"))
			return
		}
		if err := s.checkFileApproval(request); err != nil {
			fail(err)
			return
		}
		if writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}) == nil {
			s.serveApprovedFileEndpoint(c, request)
		}
		return
	}
	if len(request.Operations) != 1 || validateOperations(request.Operations) != nil {
		fail(fmt.Errorf("invalid resource operation"))
		return
	}
	op := request.Operations[0]
	switch request.Type {
	case "route_forward":
		if op.Kind != "forward" || op.LocalPortPending() {
			fail(fmt.Errorf("invalid forward request"))
			return
		}
	case "route_mount":
		if op.Kind != "mount" {
			fail(fmt.Errorf("invalid mount request"))
			return
		}
	default:
		fail(fmt.Errorf("invalid resource request"))
		return
	}
	if len(request.ResourceRoute) != 0 {
		if relay == nil {
			fail(fmt.Errorf("Windows hosted shells do not support nested routes"))
		} else if err := relay(c, request); err != nil {
			fail(err)
		}
		return
	}
	if err := s.consumeResourceApproval(request, op); err != nil {
		fail(err)
		return
	}
	if request.Type == "route_forward" {
		serveForward(c, op.Port, op.TargetPort, op.Reverse)
		return
	}
	if writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}) == nil {
		approval := files.MountApproval{
			Session: request.Name, PID: request.PID, ID: request.ApprovalID, Instance: request.Instance,
			Path: op.Path, ReadOnly: op.ReadOnly,
		}
		account, _ := localMachineIdentity()
		_ = files.ServeApprovedMount(c, version, account, beginIncomingMountTask, approval)
	}
}

// approvalLocked checks the identity of the detached shell and its pending grant.
func (s *remoteShell) approvalLocked(request message) (operation, error) {
	if s.runtime.Exited() || s.killing || s.runtime.Active() != nil || s.record.UID != request.Instance || s.record.Name != request.Name || !s.runtime.Started() || s.runtime.PID() != request.PID {
		return operation{}, fmt.Errorf("approved shell changed or remains attached; request the operation again")
	}
	grant := s.resourceApproval
	if grant.id == "" || grant.id != request.ApprovalID {
		return operation{}, fmt.Errorf("resource approval changed or was already used; request the operation again")
	}
	return grant.operations[0], nil
}

func (s *remoteShell) checkFileApproval(request message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, err := s.approvalLocked(request)
	if err == nil && op.Kind != "upload" && op.Kind != "download" {
		return fmt.Errorf("invalid file approval")
	}
	return err
}

func (s *remoteShell) consumeResourceApproval(request message, op operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	expected, err := s.approvalLocked(request)
	if err != nil {
		return err
	}
	// PTY selection sets only the origin port. The remote port and direction
	// must match the proposal retained by the source shell.
	if expected.Kind == "forward" {
		if expected.Reverse {
			expected.TargetPort = op.TargetPort
		} else {
			expected.Port = op.Port
		}
	}
	if expected != op {
		return fmt.Errorf("operation does not match the resource approval")
	}
	grant := &s.resourceApproval
	grant.operations = grant.operations[1:]
	if len(grant.operations) == 0 {
		*grant = resourceApproval{}
	}
	return nil
}

func (s *remoteShell) serveApprovedFileEndpoint(c net.Conn, request message) {
	_ = c.SetDeadline(time.Now().Add(requestTimeout))
	m, err := readMessage(c)
	if err != nil {
		return
	}
	if m.Type != "approved_transfer" || len(m.Operations) != 1 || validateOperations(m.Operations) != nil {
		_ = writeMessage(c, message{Type: "error", Error: "invalid transfer request"})
		return
	}
	op := m.Operations[0]
	if op.Kind != "upload" && op.Kind != "download" {
		_ = writeMessage(c, message{Type: "error", Error: "invalid transfer kind"})
		return
	}
	if err := s.consumeResourceApproval(request, op); err != nil {
		_ = writeMessage(c, message{Type: "error", Error: err.Error()})
		return
	}
	approvedRemoteTransfer(c, m)
}

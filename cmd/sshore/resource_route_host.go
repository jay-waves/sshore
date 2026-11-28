//go:build linux || windows

package main

import (
	"fmt"
	"net"

	"github.com/jay-waves/sshore/internal/files"
)

type resourceApproval struct {
	id         string
	operations []operation
}

func (s *tmuxTask) serveRoutedResource(c net.Conn, request message, relay func(net.Conn, message) error) {
	fail := func(err error) {
		_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
	}
	if err := checkPeerVersion(request.SoftwareVersion, "resource client"); err != nil {
		fail(err)
		return
	}
	if !validTaskUID(request.Instance) {
		fail(fmt.Errorf("resource shell changed or unavailable"))
		return
	}
	if len(request.Operations) != 1 || validateOperations(request.Operations) != nil {
		fail(fmt.Errorf("invalid resource operation"))
		return
	}
	op := request.Operations[0]
	if op.Kind == "forward" && op.Forward.LocalPort == 0 {
		fail(fmt.Errorf("invalid forward request"))
		return
	}
	if len(request.ResourceRoute) != 0 {
		if op.Kind == "upload" || op.Kind == "download" {
			fail(fmt.Errorf("invalid file route"))
			return
		}
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
	switch op.Kind {
	case "forward":
		serveForward(c, op.Forward)
	case "upload", "download":
		if err := approvedRemoteTransfer(c, request); err != nil {
			fail(err)
		}
	case "mount":
		if writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}) != nil {
			return
		}
		approval := files.MountApproval{
			Session: request.Name, PID: request.PID, ID: request.ApprovalID, Instance: request.Instance,
			Path: op.Path, ReadOnly: op.ReadOnly,
		}
		account, _ := localMachineIdentity()
		_ = files.ServeApprovedMount(c, version, account, beginIncomingMountTask, approval)
	}
}

func (s *tmuxTask) consumeResourceApproval(request message, op operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime.Exited() || s.runtime.Stopping() || s.runtime.Active() != nil || s.record.UID != request.Instance || s.record.Name != request.Name || !s.runtime.Started() || s.runtime.PID() != request.PID {
		return fmt.Errorf("approved shell changed or remains attached; request the operation again")
	}
	grant := &s.resourceApproval
	if grant.id == "" || grant.id != request.ApprovalID {
		return fmt.Errorf("resource approval changed or was already used; request the operation again")
	}
	expected := grant.operations[0]
	// Only the origin port may differ from the proposal.
	if expected.Kind == "forward" {
		expected.Forward.LocalPort = op.Forward.LocalPort
	}
	if expected != op {
		return fmt.Errorf("operation does not match the resource approval")
	}
	grant.operations = grant.operations[1:]
	if len(grant.operations) == 0 {
		*grant = resourceApproval{}
	}
	return nil
}

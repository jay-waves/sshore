//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// Validate the original detached shell without attaching to it. A failed
// validation must never terminate the shell or start a replacement session.
func checkApprovalSource(name string, pid int, approval string) error {
	if err := validSession(name); err != nil {
		return err
	}
	if pid <= 0 || !validApprovalID(approval) {
		return fmt.Errorf("invalid source approval")
	}
	gate, err := setupLock()
	if err != nil {
		return err
	}
	defer gate.Close()
	socket, err := resolveSession(name)
	if err != nil {
		return err
	}
	if socket == "" {
		return fmt.Errorf("approved shell no longer exists; request the operation again")
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err = writeMessage(conn, message{Type: "approval_check", SoftwareVersion: version, Name: name, PID: pid, ApprovalID: approval}); err != nil {
		return err
	}
	m, err := readMessage(conn)
	if err != nil {
		return err
	}
	if err = checkPeerVersion(m.SoftwareVersion, "source shell"); err != nil {
		return err
	}
	if m.Type != "approval_checked" {
		return fmt.Errorf("source approval: %s", m.Error)
	}
	return nil
}

func (s *remoteShell) checkApprovalSource(m message) error {
	if err := checkPeerVersion(m.SoftwareVersion, "approval client"); err != nil {
		return err
	}
	if !validApprovalID(m.ApprovalID) {
		return fmt.Errorf("invalid approval identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle.Exited() || s.cmd == nil || s.cmd.Process.Pid != m.PID || s.name != m.Name {
		return fmt.Errorf("approved shell changed; request the operation again")
	}
	if s.lifecycle.Active() != nil {
		return fmt.Errorf("source shell is attached; detach it before reviewing the request")
	}
	return removeRemoteTask(m.ApprovalID)
}

func runApprovedForwardWatch(args []string) error {
	if len(args) != 4 {
		return usage("forward-watch requires client version, shell, PID and approval identity")
	}
	if err := checkPeerVersion(args[0], "approval client"); err != nil {
		return err
	}
	pid, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("invalid source shell PID")
	}
	if err = checkApprovalSource(args[1], pid, args[3]); err != nil {
		return err
	}
	if _, err = fmt.Fprintln(os.Stdout, forwardReadyMarker); err != nil {
		return err
	}
	return watchRemoteTask()
}

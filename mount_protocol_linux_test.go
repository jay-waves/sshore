//go:build linux

package main

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/files"
)

// Exercise the application codec against the mount agent's narrower messages.
func TestMountAgentControlCompatibility(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan error, 1)
	go func() { done <- files.ServeAgent(a, version) }()
	defer func() {
		b.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("mount agent did not exit")
		}
	}()
	if err := writeMessage(b, message{Type: "mount_mux", SoftwareVersion: version}); err != nil {
		t.Fatal(err)
	}
	reply, err := readMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply.Error, "sftp-server executable unavailable") {
		t.Skip(reply.Error)
	}
	if reply.Type != "mount_ready" || reply.SoftwareVersion != version {
		t.Fatalf("incompatible mount reply: %+v", reply)
	}
}

func TestMountApprovalSourceGate(t *testing.T) {
	a, b := net.Pipe()
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	defer b.Close()
	called := make(chan files.MountApproval, 1)
	done := make(chan error, 1)
	go func() {
		done <- files.ServeAgent(a, version, func(source files.MountApproval) error { called <- source; return fmt.Errorf("source changed") })
	}()
	approval := &files.MountApproval{Session: "work", PID: 123, ID: strings.Repeat("a", 32)}
	if _, _, _, err := files.OpenMux(b, version, approval); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("gate bypassed: %v", err)
	}
	if got := <-called; got != *approval {
		t.Fatal(got)
	}
	if err := <-done; err == nil {
		t.Fatal("agent accepted failed approval")
	}
}

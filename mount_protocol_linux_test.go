//go:build linux

package main

import (
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

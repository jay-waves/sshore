//go:build linux

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestForwardApprovalPreservesDetachedSession(t *testing.T) {
	s := &session{name: "eevee", cmd: exec.Command("sh")}
	s.cmd.Process = &os.Process{Pid: 123}
	m := message{SoftwareVersion: version, Name: "eevee", PID: 123, ApprovalID: strings.Repeat("a", 32)}
	t.Setenv("HOME", t.TempDir())
	for _, test := range []struct {
		name   string
		change func(*message)
	}{
		{"version", func(m *message) { m.SoftwareVersion = "old" }},
		{"pid", func(m *message) { m.PID++ }},
		{"name", func(m *message) { m.Name = "renamed" }},
		{"approval", func(m *message) { m.ApprovalID = "invalid" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := m
			test.change(&bad)
			if s.checkForwardApproval(bad) == nil {
				t.Fatal("invalid approval accepted")
			}
		})
	}
	s.active = &attachment{}
	if s.checkForwardApproval(m) == nil {
		t.Fatal("attached session accepted")
	}
	s.active = nil
	if err := s.checkForwardApproval(m); err != nil {
		t.Fatal(err)
	}
	if s.active != nil || s.exited {
		t.Fatal("validation changed shell lifetime")
	}
}

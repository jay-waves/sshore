//go:build linux && sshoretest

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardStartupShowsSSHFailure(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "ssh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' 'Error: remote port forwarding failed for listen port 5173' >&2\nexit 255\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(testBinary(t), "forward", "work:5173", "origin:7777")
	cmd.Env = append(os.Environ(), "SSHORE_SOCKET=", "SSH_CONNECTION=", "SSHORE_STATE_DIR="+filepath.Join(root, "state"), "SSHORE_SSH="+script)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "remote port forwarding failed for listen port 5173") || strings.Contains(string(out), "startup failed; see") {
		t.Fatalf("native cause hidden: %v %s", err, out)
	}
}

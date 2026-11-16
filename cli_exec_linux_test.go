//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecInvokesSSHWithoutPTY(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ssh")
	argsFile := filepath.Join(dir, "args")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SSHORE_TEST_ARGS\"\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_SSH", script)
	t.Setenv("SSHORE_TEST_ARGS", argsFile)
	code, err := run([]string{"exec", "work", "printf '%s' --help"})
	if err != nil || code != 7 {
		t.Fatalf("exit %d: %v", code, err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"); len(got) != 6 || got[0] != "-T" || got[1] != "-o" || got[2] != "RequestTTY=no" || got[3] != "--" || got[4] != "work" || got[5] != "printf '%s' --help" {
		t.Fatalf("args %q", got)
	}
}

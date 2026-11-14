//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardInventoryOptional(t *testing.T) {
	for _, server := range []string{filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "different-release")} {
		if strings.HasSuffix(server, "different-release") {
			if e := os.WriteFile(server, []byte("#!/bin/sh\n[ \"$1\" = --version ] || exit 99\nprintf 'sshore 99.0.0\\n'\n"), 0700); e != nil {
				t.Fatal(e)
			}
		}
		cmd := exec.Command("/bin/sh", "-c", forwardWatchCommand(server))
		cmd.Stdin = strings.NewReader("ignored metadata\n")
		b, e := cmd.Output()
		if e != nil || string(b) != forwardReadyMarker+"\n" {
			t.Fatalf("optional inventory broke transport: %q %v", b, e)
		}
	}
}

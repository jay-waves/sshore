package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestApprovalPrompt(t *testing.T) {
	for _, scenario := range []string{"upload", "cancel", "download"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("SSHORE_SOCKET", "")
			t.Setenv("SSH_CONNECTION", "")
			root := t.TempDir()
			t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
			source := filepath.Join(root, "tank 中文 with spaces.tar.gz")
			if err := os.WriteFile(source, []byte("test"), 0600); err != nil {
				t.Fatal(err)
			}
			// Reaching this local stub proves approval proceeded, without a network call.
			stub := filepath.Join(root, "ssh-stub")
			if err := os.WriteFile(stub, []byte(`#!/bin/sh
printf 'SSH-STARTED\n' >&2
read reply </dev/tty
printf 'HANDOFF-%s\n' "$reply" >&2
exit 1
`), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SSHORE_SSH", stub)
			op := operation{Kind: "upload", Path: "/tmp"}
			if scenario == "download" {
				op = operation{Kind: "download", Path: "/tmp/new", Name: "new", Size: 4}
			}
			id, err := writeApproval(options{host: "example.invalid", session: "main", server: "sshore"}, []operation{op})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(testBinary(t), "approve", id, "--stay")
			cmd.Env = append(os.Environ(), "TERM=xterm")
			terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 160})
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()
			defer cmd.Process.Kill()
			chunks := make(chan string, 128)
			go func() {
				defer close(chunks)
				b := make([]byte, 4096)
				for {
					n, err := terminal.Read(b)
					if n > 0 {
						chunks <- string(b[:n])
					}
					if err != nil {
						return
					}
				}
			}()
			var output strings.Builder
			await := func(marker string) {
				t.Helper()
				deadline := time.NewTimer(10 * time.Second)
				defer deadline.Stop()
				for !strings.Contains(output.String(), marker) {
					select {
					case s, ok := <-chunks:
						if !ok {
							t.Fatalf("missing %q: %q", marker, output.String())
						}
						output.WriteString(s)
					case <-deadline.C:
						t.Fatalf("timed out waiting for %q: %q", marker, output.String())
					}
				}
			}
			if scenario == "download" {
				await("Save as")
				io.WriteString(terminal, filepath.Join(root, "new")+"\r")
				await("Type yes:")
				io.WriteString(terminal, "no\r")
				await("not approved")
			} else {
				await("Local file to upload")
				if scenario == "cancel" {
					io.WriteString(terminal, "\x03")
					await("approval cancelled")
				} else {
					io.WriteString(terminal, filepath.Join(root, "tank")+"\t")
					await("tank 中文 with spaces.tar.gz")
					io.WriteString(terminal, "\r")
					await("SSH-STARTED")
					io.WriteString(terminal, "ready\r")
					await("HANDOFF-ready")
					if strings.Contains(output.String(), "Type yes:") {
						t.Fatal("upload asked for redundant confirmation")
					}
				}
			}
			_ = cmd.Wait()
			dir, _ := approvalDir()
			_, err = os.Stat(filepath.Join(dir, id+".json"))
			if scenario == "upload" {
				if !os.IsNotExist(err) {
					t.Fatal("approved request not consumed", err)
				}
			} else if err != nil {
				t.Fatal("cancelled request was consumed", err)
			}
		})
	}
}

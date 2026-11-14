package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestForwardLocalSelection(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		root := t.TempDir()
		wrapper := filepath.Join(root, "ssh")
		if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf 'SSH-ARGS %s\\n' \"$*\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		args := []string{"forward", "origin:", "work:3000", "--foreground"}
		marker := "Local listening port"
		want := "-L 127.0.0.1:8080:127.0.0.1:3000"
		if reverse {
			args = []string{"forward", "work:3000", ":", "--foreground"}
			marker = "Local service port"
			want = "-R 127.0.0.1:3000:127.0.0.1:8080"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, testBinary(t), args...)
		cmd.Env = append(os.Environ(), "SSHORE_SOCKET=", "SSHORE_DISABLED=", "SSHORE_SSH="+wrapper, "TERM=xterm")
		terminal, err := pty.Start(cmd)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		var output strings.Builder
		answered := false
		buffer := make([]byte, 4096)
		for {
			n, err := terminal.Read(buffer)
			if n > 0 {
				output.Write(buffer[:n])
			}
			if !answered && strings.Contains(output.String(), marker) {
				io.WriteString(terminal, "8080\r")
				answered = true
			}
			if err != nil {
				break
			}
		}
		terminal.Close()
		err = cmd.Wait()
		cancel()
		if err != nil || !strings.Contains(output.String(), want) {
			t.Fatal(output.String(), err)
		}
	}
}

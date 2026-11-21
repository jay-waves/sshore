//go:build linux

package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/jay-waves/sshore/tests/testutil/sshtest"
	cryptossh "golang.org/x/crypto/ssh"
)

func TestForwardApprovalPreservesDetachedSession(t *testing.T) {
	activityFixture(t)
	s := &remoteShell{shellTask: shellTask{hosted: true, record: localShellRecord{Name: "eevee"}, process: processIdentity{PID: 123}}}
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
			if s.checkApprovalSource(bad) == nil {
				t.Fatal("invalid approval accepted")
			}
		})
	}
	s.active = &attachment{}
	if s.checkApprovalSource(m) == nil {
		t.Fatal("attached shell accepted")
	}
	s.active = nil
	if err := s.checkApprovalSource(m); err != nil {
		t.Fatal(err)
	}
	if s.active != nil || s.exited {
		t.Fatal("validation changed shell lifetime")
	}
}

func TestForwardLocalSelection(t *testing.T) {
	e := newEnv(t)
	for _, reverse := range []bool{false, true} {
		root := t.TempDir()
		wrapper := filepath.Join(root, "ssh")
		if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf 'SSH-ARGS %s\\n' \"$*\" >&2\nexit 7\n"), 0700); err != nil {
			t.Fatal(err)
		}
		args := []string{"forward", "here:", "work:3000", "--foreground"}
		marker := "Local listening port"
		if reverse {
			args = []string{"forward", "work:3000", ":", "--foreground"}
			marker = "Local service port"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, testBinary(t), args...)
		cmd.Env = append(e.env, "SSHORE_SOCKET=", "SSHORE_DISABLED=", "SSHORE_SSH="+wrapper, "TERM=xterm")
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
		if err == nil || !answered || !strings.Contains(output.String(), "SSH-ARGS -G") {
			t.Fatal(output.String(), err)
		}
	}
}

// Exercise reconnection over an authenticated loopback SSH endpoint.

func TestForwardReconnectAndCancel(t *testing.T) {
	activityFixture(t)
	root := t.TempDir()
	var attempts atomic.Int32
	first := make(chan struct{})
	t.Setenv("SSHORE_SSH", sshtest.Start(t, sshtest.Options{Exec: func(ctx context.Context, _ string, stream cryptossh.Channel) {
		attempt := attempts.Add(1)
		io.WriteString(stream, "sshore-forward-ready\n")
		if attempt == 1 {
			select {
			case <-first:
				sshtest.Exit(stream, 17)
			case <-ctx.Done():
			}
			return
		}
		io.Copy(io.Discard, stream)
	}}))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	_, e := resourceStateDir("forward")
	if e != nil {
		t.Fatal(e)
	}
	s := &resourceService{record: resourceRecord{Kind: "forward", Name: "web", Host: "work", Version: version}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, e := runForward(ctx, forwardConfig{Host: "work", Name: "web", Port: 8080, Target: 3000, Retries: 1}, s, nil)
		done <- e
	}()
	waitConnected := func(attempt int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for attempts.Load() != attempt || s.statusSnapshot().Transport != "connected" {
			if time.Now().After(deadline) {
				t.Fatalf("attempt %d did not connect: %+v", attempt, s.statusSnapshot())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitConnected(1)
	close(first)
	waitConnected(2)
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel stalled")
	}
}

func TestForegroundReverseForwardStartupCancellation(t *testing.T) {
	requested := make(chan struct{}, 1)
	executable := sshtest.Start(t, sshtest.Options{GlobalRequest: func(request *cryptossh.Request) {
		if request.Type == "tcpip-forward" {
			requested <- struct{}{}
			return
		}
		request.Reply(false, nil)
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := runForwardTransportForeground(ctx, executable, forwardConfig{Host: "fixture", Port: 18888, Target: 18889, Reverse: true}, io.Discard)
		result <- err
	}()
	select {
	case <-requested:
	case err := <-result:
		t.Fatalf("startup returned early: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("remote forward request missing")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled startup remained blocked waiting for remote forward reply")
	}
}

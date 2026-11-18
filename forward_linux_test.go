//go:build linux

package main

import (
	"context"
	"github.com/jay-waves/sshore/internal/sshtest"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	cryptossh "golang.org/x/crypto/ssh"
)

func TestForwardApprovalPreservesDetachedSession(t *testing.T) {
	activityFixture(t)
	s := &remoteShell{name: "eevee", shellTerminal: shellTerminal{process: processIdentity{PID: 123}}}
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
	_ = s.lifecycle.Attach(&attachment{}, func() error { return nil })
	if s.checkApprovalSource(m) == nil {
		t.Fatal("attached shell accepted")
	}
	s.lifecycle.Detach(s.lifecycle.Active())
	if err := s.checkApprovalSource(m); err != nil {
		t.Fatal(err)
	}
	if s.lifecycle.Active() != nil || s.lifecycle.Exited() {
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
		args := []string{"forward", "origin:", "work:3000", "--foreground"}
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

// Local subprocess fixture only: no SSH server, account sessions or upgrades.

func TestForwardReconnectAndCancel(t *testing.T) {
	activityFixture(t)
	root := t.TempDir()
	attempts := filepath.Join(root, "attempts")
	stub := filepath.Join(root, "ssh")
	script := "#!/bin/sh\ncount=0\n[ ! -f " + shellQuote(attempts) + " ] || count=$(cat " + shellQuote(attempts) + ")\ncount=$((count+1))\nprintf '%s' \"$count\" > " + shellQuote(attempts) + "\nprintf '%s\\n' \"$@\" >> " + shellQuote(filepath.Join(root, "args")) + "\nprintf 'sshore-forward-ready\\n'\nif [ \"$count\" = 1 ]; then sleep 0.1; exit 17; fi\nexec cat >/dev/null\n"
	if e := os.WriteFile(stub, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("SSHORE_SSH", sshtest.Wrap(t, stub, os.Environ()))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	dir, e := resourceStateDir("forward")
	if e != nil {
		t.Fatal(e)
	}
	ack := make(chan error, 1)
	ack <- nil
	s := &resourceService{record: resourceRecord{Kind: "forward", Name: "web", Host: "work", Version: version}, recordPath: filepath.Join(dir, "web.json"), ack: ack, stop: make(chan chan error), abandoned: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, e := runForward(forwardConfig{Host: "work", Name: "web", Port: 8080, Target: 3000, Retries: 1}, s)
		done <- e
	}()
	stopped := false
	defer func() {
		if !stopped {
			select {
			case s.stop <- make(chan error, 1):
			case <-done:
			case <-time.After(time.Second):
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(attempts)
		if string(b) == "2" && s.statusSnapshot().Transport == "connected" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("forward did not reconnect", s.statusSnapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	reply := make(chan error, 1)
	s.stop <- reply
	select {
	case e := <-reply:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel stalled")
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	stopped = true
	args, _ := os.ReadFile(filepath.Join(root, "args"))
	if strings.Count(string(args), "--\nfixture\n") != 2 {
		t.Fatal(string(args))
	}
}

func TestForwardRetryBoundaries(t *testing.T) {
	activityFixture(t)
	for _, scenario := range []string{"initial-failure", "exhausted", "cancel-backoff"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			attempts := filepath.Join(root, "attempts")
			stub := filepath.Join(root, "ssh")
			body := "#!/bin/sh\nprintf x >> " + shellQuote(attempts) + "\n"
			if scenario != "initial-failure" {
				body += "printf 'sshore-forward-ready\\n'\nsleep 0.05\n"
			}
			body += "exit 17\n"
			if e := os.WriteFile(stub, []byte(body), 0700); e != nil {
				t.Fatal(e)
			}
			t.Setenv("SSHORE_SSH", sshtest.Wrap(t, stub, os.Environ()))
			t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
			dir, e := resourceStateDir("forward")
			if e != nil {
				t.Fatal(e)
			}
			ack := make(chan error, 1)
			ack <- nil
			s := &resourceService{record: resourceRecord{Kind: "forward", Name: "web", Host: "work", Version: version}, recordPath: filepath.Join(dir, "web.json"), ack: ack, stop: make(chan chan error), abandoned: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				_, e := runForward(forwardConfig{Host: "work", Name: "web", Port: 8080, Target: 3000, Retries: 1}, s)
				done <- e
			}()
			if scenario == "cancel-backoff" {
				deadline := time.Now().Add(time.Second)
				for s.statusSnapshot().Transport != "reconnecting" {
					if time.Now().After(deadline) {
						t.Fatal("missing retry backoff")
					}
					time.Sleep(time.Millisecond)
				}
				reply := make(chan error, 1)
				s.stop <- reply
				select {
				case <-reply:
				case <-time.After(time.Second):
					t.Fatal("backoff cancellation stalled")
				}
			}
			select {
			case e := <-done:
				if (e == nil) != (scenario == "cancel-backoff") {
					t.Fatal(e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("retry limit ignored")
			}
			b, _ := os.ReadFile(attempts)
			want := 1
			if scenario == "exhausted" {
				want = 2
			}
			if len(b) != want {
				t.Fatal("unexpected attempts", string(b))
			}
		})
	}
}

func TestForegroundReverseForwardStartupCancellation(t *testing.T) {
	requested := make(chan struct{}, 1)
	executable := sshtest.Wrap(t, "unused", nil, func(request *cryptossh.Request) {
		if request.Type == "tcpip-forward" {
			requested <- struct{}{}
			return
		}
		request.Reply(false, nil)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := runForwardTransportForeground(ctx, executable, forwardConfig{Host: "fixture", Port: 18888, Target: 18889, Reverse: true}, nil, io.Discard, io.Discard)
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

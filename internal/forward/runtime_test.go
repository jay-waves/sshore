package forward

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestForwardDiagnosticsBounded(t *testing.T) {
	var d diagnosticsTail
	d.Write([]byte(strings.Repeat("x", 8192)))
	d.Write([]byte("\nError: remote port forwarding failed for listen port 5173\n"))
	if len(d.data) > 4096 || !strings.Contains(d.String(), "listen port 5173") {
		t.Fatalf("diagnostic tail lost: %q", d.String())
	}
}

func TestTransportArgumentsAndConfig(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		c := Config{Host: "work", Port: 8080, Target: 3000, Reverse: reverse, SSH: []string{"-p", "2222", "-L", "9999:localhost:9"}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		args := SSHArgs(c, "opaque helper")
		joined := strings.Join(args, " ")
		flag := "-L"
		if reverse {
			flag = "-R"
		}
		if !strings.Contains(joined, flag+" 127.0.0.1:8080:127.0.0.1:3000") || strings.Contains(joined, "9999") || args[len(args)-1] != "opaque helper" {
			t.Fatal(args)
		}
		foreground := SSHArgs(c, "")
		if foreground[len(foreground)-1] != "work" || !strings.Contains(strings.Join(foreground, " "), "SessionType=none") {
			t.Fatal(foreground)
		}
	}
	for _, c := range []Config{{Port: 0, Target: 80}, {Port: 80, Target: 65536}, {Port: 80, Target: 80, Retries: 11}} {
		if c.Validate() == nil {
			t.Fatal("invalid config accepted", c)
		}
	}
}

func TestForwardTransportDoesNotReplayOtherRules(t *testing.T) {
	args := SSHArgs(Config{Host: "work", Port: 8080, Target: 3000, SSH: []string{"-p", "2222", "-L", "9999:localhost:9999", "-R", "8888:localhost:8888", "-D", "7777", "-o", "RemoteForward=6666 localhost:66", "-o", "LocalForward 5555 localhost:55"}}, "opaque helper")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "9999") || strings.Contains(joined, "8888") || strings.Contains(joined, "7777") || strings.Contains(joined, "6666") || strings.Contains(joined, "5555") || !strings.Contains(joined, "-p 2222") || !strings.Contains(joined, "-L 127.0.0.1:8080:127.0.0.1:3000") {
		t.Fatal(args)
	}
}

func TestRetryPolicy(t *testing.T) {
	for _, value := range []string{"0", "1", "10"} {
		n, err := ParseRetries(value)
		if err != nil || (Config{Retries: n, Port: 80, Target: 80}).Validate() != nil {
			t.Fatalf("%q: %d %v", value, n, err)
		}
	}
	for _, value := range []string{"", "-1", "11", "invalid", "9999999999999999999999"} {
		if _, err := ParseRetries(value); err != ErrRetries {
			t.Fatalf("%q: %v", value, err)
		}
	}
}

func TestBlockedHeartbeatCancellation(t *testing.T) {
	if os.Getenv("SSHORE_FORWARD_BLOCKED_HELPER") == "1" {
		fmt.Fprintln(os.Stdout, ReadyMarker)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heartbeat := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, Runtime{
			Log: io.Discard, HeartbeatInterval: time.Hour,
			Command: func(ctx context.Context, _ bool) *exec.Cmd {
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBlockedHeartbeatCancellation$")
				cmd.Env = append(os.Environ(), "SSHORE_FORWARD_BLOCKED_HELPER=1")
				return cmd
			},
			Ready: func() error { return nil }, Status: func(string, error) {},
			Heartbeat: func() ([]byte, error) { close(heartbeat); return bytes.Repeat([]byte("x"), 1<<20), nil },
		})
		done <- err
	}()
	select {
	case <-heartbeat:
	case err := <-done:
		t.Fatalf("exited before heartbeat: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup stalled")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not join blocked heartbeat and process")
	}
}

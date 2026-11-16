//go:build sshoretest

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestBlockedHeartbeatCancellation(t *testing.T) {
	if os.Getenv("SSHORE_FORWARD_BLOCKED_HELPER") == "1" {
		fmt.Fprintln(os.Stdout, forwardReadyMarker)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heartbeat := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runForwardTransport(ctx, forwardTransport{
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

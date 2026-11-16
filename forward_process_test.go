//go:build sshoretest

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestBlockedHeartbeatCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heartbeat := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runForwardTransport(ctx, forwardTransport{
			Log: io.Discard, HeartbeatInterval: time.Hour,
			Start: func(ctx context.Context, _ bool, _ io.Writer) (*forwardConnection, error) {
				client, server := net.Pipe()
				go func() { io.WriteString(server, forwardReadyMarker+"\n"); <-ctx.Done(); server.Close() }()
				stop := context.AfterFunc(ctx, func() { client.Close() })
				return &forwardConnection{Input: client, Output: client, Close: func() error { stop(); return client.Close() }, Wait: func() error { <-ctx.Done(); return ctx.Err() }}, nil
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

package main

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

func TestTerminalKeyboardStopReleasesInput(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	received := make(chan string, 1)
	stop, err := startTerminalKeyboardFile(context.Background(), input, func(data []byte, err error) {
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		received <- string(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte("old")); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-received:
		if data != "old" {
			t.Fatal(data)
		}
	case <-time.After(time.Second):
		t.Fatal("keyboard data not delivered")
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("keyboard stop did not join")
	}
	// No old reader can steal subsequent authentication/session input.
	if _, err = writer.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3)
	if _, err = io.ReadFull(input, data); err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("next reader got %q", data)
	}
	stop()
}

func TestTerminalKeyboardParentCancelAndConcurrentStop(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stop, err := startTerminalKeyboardFile(ctx, input, func([]byte, error) { t.Error("silent keyboard emitted data") })
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(stop)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled reader remained live")
	}
}

package main

import (
	"context"
	"golang.org/x/sys/windows"
	"sync"
	"testing"
	"time"
)

type synchronousKeyboardPipe struct {
	handle  windows.Handle
	started chan struct{}
	once    sync.Once
}

func (r *synchronousKeyboardPipe) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	var n uint32
	err := windows.ReadFile(r.handle, p, &n, nil)
	return int(n), err
}

func TestTerminalKeyboardCancelsSynchronousRead(t *testing.T) {
	var read, write windows.Handle
	if err := windows.CreatePipe(&read, &write, nil, 4096); err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(read)
	defer windows.CloseHandle(write)
	reader := &synchronousKeyboardPipe{handle: read, started: make(chan struct{})}
	stop, err := startTerminalKeyboardReader(context.Background(), func([]byte, error) { t.Error("cancelled read emitted data") }, reader)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("synchronous keyboard cancellation did not join")
	}
	// Cancellation did not close the shared input handle.
	var n uint32
	if err = windows.WriteFile(write, []byte("next"), &n, nil); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if err = windows.ReadFile(read, buf, &n, nil); err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "next" {
		t.Fatalf("next reader got %q", buf[:n])
	}
	stop()
}

func TestTerminalKeyboardAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop, err := startTerminalKeyboard(ctx, func([]byte, error) { t.Error("read after cancelled start") })
	if err != nil {
		t.Fatal(err)
	}
	stop()
}

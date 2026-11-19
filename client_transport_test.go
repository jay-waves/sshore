package main

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestClientTransportCancellationAndConcurrentClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, precancel := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			if precancel {
				cancel()
			}
			a, b := net.Pipe()
			var calls atomic.Int32
			c := ownClientTransport(ctx, a, func() { calls.Add(1) })
			var wg sync.WaitGroup
			for range 16 {
				wg.Go(func() { c.Close() })
			}
			cancel()
			wg.Wait()
			b.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := b.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("owner did not close stream: %v", err)
			}
			b.Close()
			if calls.Load() != 1 {
				t.Fatalf("cleanup calls = %d", calls.Load())
			}
		}
	})
}

func TestClientTransportCancellationUnblocksIO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		a, b := net.Pipe()
		defer b.Close()
		c := ownClientTransport(ctx, a, nil)
		defer c.Close()
		done := make(chan struct{})
		go func() { defer close(done); _, _ = c.Read(make([]byte, 1)) }()
		// Establish a blocked reader without depending on scheduler timing.
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("read ended before cancellation")
		default:
		}
		// An hour of virtual time exercises the actual context deadline.
		time.Sleep(time.Hour)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("deadline did not unblock read")
		}
	})
}

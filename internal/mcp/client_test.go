package mcp

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientReusesTransportAndDoesNotReplay(t *testing.T) {
	var dials atomic.Int32
	var requests atomic.Int32
	c := &Client{Version: "1.2.3", Dial: func(context.Context, string) (io.ReadWriteCloser, error) {
		dials.Add(1)
		local, remote := net.Pipe()
		go func() {
			defer remote.Close()
			for {
				req, err := ReadRequest(remote)
				if err != nil {
					return
				}
				requests.Add(1)
				if req.Method == "execute" {
					return
				}
				if WriteResponse(remote, Response{Version: "1.2.3", Sessions: &Sessions{}}) != nil {
					return
				}
			}
		}()
		return local, nil
	}}
	defer c.Shutdown()
	ctx := context.Background()
	for range 2 {
		if _, err := c.List(ctx, OpenInput{Host: "work"}); err != nil {
			t.Fatal(err)
		}
	}
	if dials.Load() != 1 {
		t.Fatal("transport was not reused")
	}
	if _, err := c.call(ctx, "work", Request{Method: "execute"}); err == nil {
		t.Fatal("missing ambiguous failure")
	}
	if requests.Load() != 3 || dials.Load() != 1 {
		t.Fatal("request replayed")
	}
	if _, err := c.List(ctx, OpenInput{Host: "work"}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 2 {
		t.Fatal("failed transport was not replaced")
	}
}

func TestClientTransportOutlivesRequest(t *testing.T) {
	var transportCtx context.Context
	var dials atomic.Int32
	c := &Client{Version: "1.2.3", Dial: func(ctx context.Context, _ string) (io.ReadWriteCloser, error) {
		transportCtx = ctx
		dials.Add(1)
		local, remote := net.Pipe()
		go func() {
			defer remote.Close()
			for {
				if _, err := ReadRequest(remote); err != nil {
					return
				}
				if WriteResponse(remote, Response{Version: "1.2.3", Sessions: &Sessions{}}) != nil {
					return
				}
			}
		}()
		return local, nil
	}}
	defer c.Shutdown()
	request, cancel := context.WithCancel(context.Background())
	if _, err := c.List(request, OpenInput{Host: "work"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if transportCtx.Err() != nil {
		t.Fatal("request cancellation ended pooled transport")
	}
	if _, err := c.List(context.Background(), OpenInput{Host: "work"}); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatal("transport was not reused")
	}
	c.Shutdown()
	if transportCtx.Err() == nil {
		t.Fatal("shutdown did not end transport lifetime")
	}
	if _, err := c.List(context.Background(), OpenInput{Host: "work"}); err == nil {
		t.Fatal("closed client accepted a request")
	}
}

func TestClientShutdownCancelsDial(t *testing.T) {
	ready := make(chan struct{})
	c := &Client{Version: "1.2.3", Dial: func(ctx context.Context, _ string) (io.ReadWriteCloser, error) {
		close(ready)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	done := make(chan error, 1)
	go func() { _, err := c.List(context.Background(), OpenInput{Host: "work"}); done <- err }()
	<-ready
	c.Shutdown()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown left dial running")
	}
}

func TestClientExpandsTransportCapacityForConcurrentCalls(t *testing.T) {
	var dials atomic.Int32
	started := make(chan struct{}, maxHostConnections+1)
	release := make(chan struct{})
	c := &Client{Version: "1.2.3", Dial: func(context.Context, string) (io.ReadWriteCloser, error) {
		dials.Add(1)
		local, remote := net.Pipe()
		go func() {
			defer remote.Close()
			for {
				if _, err := ReadRequest(remote); err != nil {
					return
				}
				started <- struct{}{}
				<-release
				if WriteResponse(remote, Response{Version: "1.2.3", Sessions: &Sessions{}}) != nil {
					return
				}
			}
		}()
		return local, nil
	}}
	defer c.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := make(chan error, maxHostConnections+1)
	for range maxHostConnections + 1 {
		go func() { _, err := c.List(ctx, OpenInput{Host: "work"}); results <- err }()
	}
	for range maxHostConnections {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("concurrent calls did not obtain independent transports")
		}
	}
	close(release)
	for range maxHostConnections + 1 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := dials.Load(); got != maxHostConnections {
		t.Fatalf("dialed %d transports, want bounded expansion to %d", got, maxHostConnections)
	}
}

func TestClientPoolCancellationDoesNotCloseOtherTransports(t *testing.T) {
	started := make(chan struct{}, 2)
	finish := make(chan struct{}, 1)
	defer close(finish)
	c := &Client{Version: "1.2.3", Dial: func(context.Context, string) (io.ReadWriteCloser, error) {
		local, remote := net.Pipe()
		go func() {
			defer remote.Close()
			req, err := ReadRequest(remote)
			if err != nil {
				return
			}
			started <- struct{}{}
			if req.Method == "slow" {
				_, _ = remote.Read(make([]byte, 1))
				return
			}
			<-finish
			_ = WriteResponse(remote, Response{Version: "1.2.3", Sessions: &Sessions{}})
		}()
		return local, nil
	}}
	defer c.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	slow, cancelSlow := context.WithCancel(ctx)
	defer cancelSlow()
	slowDone, otherDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := c.call(slow, "work", Request{Method: "slow"}); slowDone <- err }()
	go func() { _, err := c.List(ctx, OpenInput{Host: "work"}); otherDone <- err }()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("calls did not run concurrently")
		}
	}
	cancelSlow()
	if err := <-slowDone; err == nil {
		t.Fatal("canceled request succeeded")
	}
	finish <- struct{}{}
	if err := <-otherDone; err != nil {
		t.Fatalf("another transport was canceled: %v", err)
	}
}

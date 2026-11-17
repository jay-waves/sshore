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

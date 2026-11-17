package mcp

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
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

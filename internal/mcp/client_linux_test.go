//go:build linux

package mcp

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestClientReconnectsToSession(t *testing.T) {
	ctx := context.Background()
	remote := NewSessionsManager()
	dial := func(_ context.Context, host string) (io.ReadWriteCloser, error) {
		if host != "work" {
			t.Fatalf("unexpected host %q", host)
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			for {
				req, err := ReadRequest(server)
				if err != nil {
					return
				}
				if WriteResponse(server, Dispatch(ctx, remote, req, "1.2.3")) != nil {
					return
				}
			}
		}()
		return client, nil
	}
	first := Client{Version: "1.2.3", Dial: dial}
	session, err := first.Open(ctx, OpenInput{Host: "work"})
	if err != nil {
		t.Fatal(err)
	}
	second := Client{Version: "1.2.3", Dial: dial}
	defer second.Close(ctx, SessionInput{SessionID: session.ID})
	sessions, err := second.List(ctx, OpenInput{Host: "work"})
	if err != nil || len(sessions.Sessions) != 1 || sessions.Sessions[0].ID != session.ID {
		t.Fatalf("%+v %v", sessions, err)
	}
	execute := func(code string) string {
		t.Helper()
		run, err := second.Execute(ctx, ExecuteInput{SessionID: session.ID, Code: code})
		if err != nil {
			t.Fatal(err)
		}
		cursor := ""
		var output strings.Builder
		deadline := time.Now().Add(3 * time.Second)
		for {
			result, err := second.Read(ctx, ReadInput{SessionID: session.ID, ExecutionID: run.ID, Cursor: cursor, WaitMS: 10})
			if err != nil {
				t.Fatal(err)
			}
			output.WriteString(result.Stdout)
			cursor = result.Cursor
			if result.State == "completed" && result.OutputClosed {
				return output.String()
			}
			if time.Now().After(deadline) {
				t.Fatal("timeout")
			}
			time.Sleep(time.Millisecond)
		}
	}
	execute("x=retained")
	if got := execute("printf '%s' \"$x\""); got != "retained" {
		t.Fatalf("got %q", got)
	}
}

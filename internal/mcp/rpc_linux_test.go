//go:build linux

package mcp

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestRPCSessions(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	manager := NewSessionsManager()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		for {
			request, err := ReadRequest(server)
			if err != nil {
				return
			}
			reply := Dispatch(context.Background(), manager, request, "1.2.3")
			if err := WriteResponse(server, reply); err != nil {
				return
			}
		}
	}()
	send := func(req Request) Response {
		t.Helper()
		req.Version = "1.2.3"
		if err := WriteRequest(client, req); err != nil {
			t.Fatal(err)
		}
		response, err := ReadResponse(client)
		if err != nil || response.Error != "" || response.Version != "1.2.3" {
			t.Fatalf("response %+v: %v", response, err)
		}
		return response
	}
	s := send(Request{Method: "open", Open: OpenInput{Host: "work"}}).Session
	e := send(Request{Method: "execute", Execute: ExecuteInput{SessionID: s.ID, Code: "a=42; printf '%s' \"$a\""}}).Execution
	var result *Output
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		result = send(Request{Method: "read", Read: ReadInput{SessionID: s.ID, ExecutionID: e.ID, WaitMS: 10}}).Output
		if result.OutputClosed && result.State == "completed" {
			break
		}
	}
	if result == nil || result.Stdout != "42" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("output %+v", result)
	}
	send(Request{Method: "close", Session: SessionInput{SessionID: s.ID}})
	client.Close()
	<-done
}

func TestRPCRejectsMismatchedRelease(t *testing.T) {
	m := NewSessionsManager()
	result := Dispatch(context.Background(), m, Request{Version: "1.2.2", Method: "open", Open: OpenInput{Host: "work"}}, "1.2.3")
	if result.Error == "" || result.Session != nil {
		t.Fatalf("%+v", result)
	}
	sessions, _ := m.List(context.Background(), OpenInput{})
	if len(sessions.Sessions) != 0 {
		t.Fatal("mismatched request created a session")
	}
}

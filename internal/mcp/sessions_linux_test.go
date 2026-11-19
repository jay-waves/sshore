//go:build linux

package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// The protocol fixtures run without the application daemon's task registry.
type sessionFixture struct {
	mu       sync.Mutex
	sessions map[string]*BashSession
}

func newSessionFixture() *sessionFixture {
	return &sessionFixture{sessions: make(map[string]*BashSession)}
}

func (f *sessionFixture) Open(ctx context.Context, in OpenInput) (Session, error) {
	bash, err := StartBashSession(ctx)
	if err != nil {
		return Session{}, err
	}
	id := rand.Text()
	f.mu.Lock()
	f.sessions[id] = bash
	f.mu.Unlock()
	return Session{ID: id, Host: in.Host, State: "ready"}, nil
}

func (f *sessionFixture) List(_ context.Context, in OpenInput) (Sessions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := Sessions{Sessions: make([]Session, 0, len(f.sessions))}
	for id, bash := range f.sessions {
		result.Sessions = append(result.Sessions, Session{ID: id, Host: in.Host, State: bash.State()})
	}
	return result, nil
}

func (f *sessionFixture) session(id string) *BashSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[id]
}

func (f *sessionFixture) Execute(ctx context.Context, in ExecuteInput) (Execution, error) {
	if bash := f.session(in.SessionID); bash != nil {
		return bash.Execute(ctx, in)
	}
	return Execution{}, fmt.Errorf("session not found")
}

func (f *sessionFixture) Read(ctx context.Context, in ReadInput) (Output, error) {
	if bash := f.session(in.SessionID); bash != nil {
		return bash.Read(ctx, in)
	}
	return Output{}, fmt.Errorf("session not found")
}

func (f *sessionFixture) Close(_ context.Context, in SessionInput) (Closed, error) {
	f.mu.Lock()
	bash := f.sessions[in.SessionID]
	delete(f.sessions, in.SessionID)
	f.mu.Unlock()
	if bash == nil {
		return Closed{}, fmt.Errorf("session not found")
	}
	err := bash.Close()
	return Closed{Closed: err == nil}, err
}

func TestSessionsRetainBashState(t *testing.T) {
	ctx := context.Background()
	m := newSessionFixture()
	s, err := m.Open(ctx, OpenInput{Host: "host"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx, SessionInput{SessionID: s.ID})
	run := func(script, stdin string) Output {
		t.Helper()
		e, err := m.Execute(ctx, ExecuteInput{SessionID: s.ID, Code: script, Stdin: stdin})
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		var text strings.Builder
		cursor := ""
		for {
			r, err := m.Read(ctx, ReadInput{SessionID: s.ID, ExecutionID: e.ID, Cursor: cursor, WaitMS: 10})
			if err != nil {
				t.Fatal(err)
			}
			text.WriteString(r.Stdout)
			cursor = r.Cursor
			if r.State == "completed" && r.OutputClosed {
				r.Stdout = text.String()
				return r
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %+v", r)
			}
			time.Sleep(time.Millisecond)
		}
	}
	r := run("x=world; read -r answer; printf '%s' \"$answer\"", "hello\n")
	if r.Stdout != "hello" || r.ExitCode == nil || *r.ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	r = run("printf '%s' \"$x\"", "")
	if r.Stdout != "world" {
		t.Fatalf("%+v", r)
	}
}

func TestSessionsBoundedOutputAndBinary(t *testing.T) {
	ctx := context.Background()
	m := newSessionFixture()
	session, err := m.Open(ctx, OpenInput{Host: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx, SessionInput{SessionID: session.ID})
	execution, err := m.Execute(ctx, ExecuteInput{SessionID: session.ID, Code: "head -c 10000 /dev/zero"})
	if err != nil {
		t.Fatal(err)
	}
	var received []byte
	cursor := ""
	deadline := time.Now().Add(3 * time.Second)
	for {
		result, err := m.Read(ctx, ReadInput{SessionID: session.ID, ExecutionID: execution.ID, Cursor: cursor, WaitMS: 10})
		if err != nil {
			t.Fatal(err)
		}
		if result.StdoutBase64 != "" {
			b, err := base64.StdEncoding.DecodeString(result.StdoutBase64)
			if err != nil {
				t.Fatal(err)
			}
			received = append(received, b...)
		} else {
			received = append(received, []byte(result.Stdout)...)
		}
		cursor = result.Cursor
		if result.State == "completed" && result.OutputClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out at %s", cursor)
		}
		time.Sleep(time.Millisecond)
	}
	if len(received) != 10000 || bytes.Count(received, []byte{0}) != 10000 {
		t.Fatalf("received %d bytes", len(received))
	}
}

func TestSessionsReportsUnknownAndPreservesOutput(t *testing.T) {
	ctx := context.Background()
	m := newSessionFixture()
	s, err := m.Open(ctx, OpenInput{Host: "host"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx, SessionInput{SessionID: s.ID})
	run, err := m.Execute(ctx, ExecuteInput{SessionID: s.ID, Code: "printf before; exit 9"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		out, err := m.Read(ctx, ReadInput{SessionID: s.ID, ExecutionID: run.ID, WaitMS: 10})
		if err != nil {
			t.Fatal(err)
		}
		if out.State == "unknown" && out.OutputClosed {
			if out.Stdout != "before" || out.Error == "" {
				t.Fatalf("%+v", out)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("unknown execution status not reported")
}

func TestReadWaitReturnsOnOutput(t *testing.T) {
	ctx := context.Background()
	m := newSessionFixture()
	s, err := m.Open(ctx, OpenInput{Host: "host"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx, SessionInput{SessionID: s.ID})
	run, err := m.Execute(ctx, ExecuteInput{SessionID: s.ID, Code: "sleep .1; printf ready; sleep .4"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		result, err := m.Read(ctx, ReadInput{SessionID: s.ID, ExecutionID: run.ID, WaitMS: 1000})
		if err != nil {
			t.Fatal(err)
		}
		if result.Stdout == "ready" {
			if result.State != "running" {
				t.Fatalf("output did not wake reader before completion: %+v", result)
			}
			return
		}
	}
	t.Fatal("no output before deadline")
}

func TestCompletedExecutionEviction(t *testing.T) {
	ctx := context.Background()
	manager := newSessionFixture()
	session, err := manager.Open(ctx, OpenInput{Host: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(ctx, SessionInput{SessionID: session.ID})
	first := ""
	for i := 0; i < 17; i++ {
		run, err := manager.Execute(ctx, ExecuteInput{SessionID: session.ID, Code: ":"})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = run.ID
		}
		deadline := time.Now().Add(time.Second)
		for {
			result, err := manager.Read(ctx, ReadInput{SessionID: session.ID, ExecutionID: run.ID, WaitMS: 20})
			if err != nil {
				t.Fatal(err)
			}
			if result.State == "completed" && result.OutputClosed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timeout")
			}
		}
	}
	if _, err := manager.Read(ctx, ReadInput{SessionID: session.ID, ExecutionID: first}); err == nil {
		t.Fatal("old completed execution was not evicted")
	}
}

func TestExitedSessionIsNotReportedReady(t *testing.T) {
	ctx := context.Background()
	m := newSessionFixture()
	s, err := m.Open(ctx, OpenInput{Host: "host"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx, SessionInput{SessionID: s.ID})
	run, err := m.Execute(ctx, ExecuteInput{SessionID: s.ID, Code: "exit"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		list, err := m.List(ctx, OpenInput{Host: "host"})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Sessions) == 1 && list.Sessions[0].State == "exited" {
			if _, err = m.Execute(ctx, ExecuteInput{SessionID: s.ID, Code: ":"}); err == nil {
				t.Fatal("exited session accepted execution")
			}
			if _, err = m.Read(ctx, ReadInput{SessionID: s.ID, ExecutionID: run.ID}); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("exited session reported ready")
}

func TestRPCSessions(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	manager := newSessionFixture()
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
	m := newSessionFixture()
	result := Dispatch(context.Background(), m, Request{Version: "1.2.2", Method: "open", Open: OpenInput{Host: "work"}}, "1.2.3")
	if result.Error == "" || result.Session != nil {
		t.Fatalf("%+v", result)
	}
	sessions, _ := m.List(context.Background(), OpenInput{})
	if len(sessions.Sessions) != 0 {
		t.Fatal("mismatched request created a session")
	}
}

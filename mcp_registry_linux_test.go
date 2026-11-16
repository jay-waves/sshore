//go:build linux && sshoretest

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"

	"github.com/jay-waves/sshore/internal/mcp"
	"path/filepath"
	"testing"
	"time"
)

func TestMCPRegistryHealth(t *testing.T) {
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	entry, err := registerMCPSession("ABC123")
	if err != nil {
		t.Fatal(err)
	}
	path := entry.path
	live, err := sessionIdentityLive(path)
	if err != nil || !live {
		t.Fatalf("live=%v error=%v", live, err)
	}
	status := probeSessionDaemon(daemonProcess{pid: os.Getpid(), socket: path, shared: true}, time.Now().Add(time.Second))
	if status.state != "detached" || status.name != "mcp-ABC123" {
		t.Fatalf("health: %+v", status)
	}
	if _, err := registerMCPSession("ABC123"); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	entry.Close()
	live, err = sessionIdentityLive(path)
	if err != nil || live {
		t.Fatalf("after close live=%v error=%v", live, err)
	}
}

func TestTrackedMCPSessionRegistration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	sessions := newTrackedMCPSessions()
	session, err := sessions.Open(context.Background(), mcp.OpenInput{Host: "work"})
	if err != nil {
		t.Fatal(err)
	}
	path, err := socketPath("mcp-" + session.ID)
	if err != nil {
		t.Fatal(err)
	}
	live, err := sessionIdentityLive(path)
	if err != nil || !live {
		t.Fatalf("missing registration: %v %v", live, err)
	}
	_, err = sessions.Close(context.Background(), mcp.SessionInput{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	live, err = sessionIdentityLive(path)
	if err != nil || live {
		t.Fatalf("registration remains: %v %v", live, err)
	}
}

func TestSharedDaemonMCPSession(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	done := make(chan error, 1)
	go func() { done <- runSharedRemoteDaemonUsing(nil) }()
	path := filepath.Join(root, "runtime", "daemon.control")
	var conn net.Conn
	var err error
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); {
		conn, err = net.DialTimeout("unix", path, 50*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	call := func(req mcp.Request) mcp.Response {
		t.Helper()
		req.Version = version
		c, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err = writeMessage(c, message{Type: "mcp_request", SoftwareVersion: version, MCPRequest: &req}); err != nil {
			t.Fatal(err)
		}
		reply, err := readMessage(c)
		if err != nil || reply.MCPResponse == nil || reply.MCPResponse.Error != "" {
			t.Fatalf("reply %+v: %v", reply, err)
		}
		return *reply.MCPResponse
	}
	session := call(mcp.Request{Method: "open", Open: mcp.OpenInput{Host: "host"}}).Session
	execution := call(mcp.Request{Method: "execute", Execute: mcp.ExecuteInput{SessionID: session.ID, Code: "printf daemon"}}).Execution
	var output *mcp.Output
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		output = call(mcp.Request{Method: "read", Read: mcp.ReadInput{SessionID: session.ID, ExecutionID: execution.ID, WaitMS: 20}}).Output
		if output.State == "completed" && output.OutputClosed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if output == nil || output.Stdout != "daemon" {
		t.Fatalf("output %+v", output)
	}
	call(mcp.Request{Method: "close", Session: mcp.SessionInput{SessionID: session.ID}})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not exit")
	}
}

func TestTrackedMCPCloseStopsChild(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	sessions := newTrackedMCPSessions()
	ctx := context.Background()
	session, err := sessions.Open(ctx, mcp.OpenInput{Host: "work"})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := sessions.Execute(ctx, mcp.ExecuteInput{SessionID: session.ID, Code: "sleep 30 & printf '%s' \"$!\""})
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		output, err := sessions.Read(ctx, mcp.ReadInput{SessionID: session.ID, ExecutionID: execution.ID})
		if err != nil {
			t.Fatal(err)
		}
		if output.Stdout != "" {
			pid, err = strconv.Atoi(output.Stdout)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child PID unavailable")
	}
	child, err := readSetupTask(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = signalProcess(pid, child.start, 9) }()
	if _, err = sessions.Close(ctx, mcp.SessionInput{SessionID: session.ID}); err != nil {
		t.Fatal(err)
	}
	if now, err := readSetupTask(pid); err == nil && now.start == child.start && now.state != "Z" {
		t.Fatal("child still running")
	}
}

func TestTrackedMCPCloseAfterBashExit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	sessions := newTrackedMCPSessions()
	ctx := context.Background()
	session, err := sessions.Open(ctx, mcp.OpenInput{Host: "work"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := sessions.Execute(ctx, mcp.ExecuteInput{SessionID: session.ID, Code: "exit 9"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		output, err := sessions.Read(ctx, mcp.ReadInput{SessionID: session.ID, ExecutionID: run.ID, WaitMS: 20})
		if err != nil {
			t.Fatal(err)
		}
		if output.State == "unknown" {
			break
		}
	}
	if _, err = sessions.Close(ctx, mcp.SessionInput{SessionID: session.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestTrackedMCPOpenRechecksSetup(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	sessions := newTrackedMCPSessions(func() error { return errors.New("setup changed") })
	if _, err := sessions.Open(context.Background(), mcp.OpenInput{Host: "work"}); err == nil {
		t.Fatal("changed setup accepted")
	}
	list, err := sessions.List(context.Background(), mcp.OpenInput{Host: "work"})
	if err != nil || len(list.Sessions) != 0 {
		t.Fatalf("%+v %v", list, err)
	}
}

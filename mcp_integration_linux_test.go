package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jay-waves/sshore/internal/mcp"
	"github.com/jay-waves/sshore/internal/proc"
	"github.com/jay-waves/sshore/internal/sshtest"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestMCPStdioReconnect(t *testing.T) {
	e := newEnv(t)
	wrapper := filepath.Join(e.dir, "ssh-wrapper")
	script := "#!/bin/sh\nexec \"$SSHORE_TEST_REMOTE_BIN\" mcp-agent " + shellQuote(version) + "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	wrapper = sshtest.Wrap(t, wrapper, append(e.env, "SSHORE_TEST_REMOTE_BIN="+e.bin))
	env := append(append([]string{}, e.env...), "SSHORE_SSH="+wrapper, "SSHORE_TEST_REMOTE_BIN="+e.bin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connect := func() *sdk.ClientSession {
		t.Helper()
		cmd := exec.Command(e.bin, "mcp")
		cmd.Env = env
		cmd.Stderr = os.Stderr
		c, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	call := func(c *sdk.ClientSession, name string, args any, output any) {
		t.Helper()
		res, err := c.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
		data, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, output); err != nil {
			t.Fatal(err)
		}
	}
	c := connect()
	var session mcp.Session
	call(c, "session_open", map[string]any{"host": "test"}, &session)
	var execution mcp.Execution
	call(c, "session_execute", map[string]any{"session_id": session.ID, "code": "x=retained"}, &execution)
	wait := func(client *sdk.ClientSession, executionID string) string {
		t.Helper()
		cursor := ""
		text := ""
		for {
			var result mcp.Output
			call(client, "session_read", map[string]any{"session_id": session.ID, "execution_id": executionID, "cursor": cursor, "wait_ms": 20}, &result)
			text += result.Stdout
			cursor = result.Cursor
			if result.State == "completed" && result.OutputClosed {
				return text
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			default:
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait(c, execution.ID)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = connect()
	defer c.Close()
	var sessions mcp.Sessions
	call(c, "session_list", map[string]any{"host": "test"}, &sessions)
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].ID != session.ID {
		t.Fatalf("sessions %+v", sessions)
	}
	call(c, "session_execute", map[string]any{"session_id": session.ID, "code": "printf '%s' \"$x\""}, &execution)
	if got := wait(c, execution.ID); got != "retained" {
		t.Fatalf("output %q", got)
	}
	var closed mcp.Closed
	call(c, "session_close", map[string]any{"session_id": session.ID}, &closed)
	if !closed.Closed {
		t.Fatal("session not closed")
	}
}

func TestTrackedMCPSessionRegistration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	sessions := newTrackedMCPSessions(&daemonTasks{})
	session, err := sessions.Open(context.Background(), mcp.OpenInput{Host: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if !validTaskUID(session.ID) || !sessions.tasks.Busy() {
		t.Fatal("missing task registration")
	}
	if _, err := os.Stat(filepath.Join(root, "runtime")); !os.IsNotExist(err) {
		t.Fatalf("MCP registration created runtime files: %v", err)
	}
	if _, err := sessions.Close(context.Background(), mcp.SessionInput{SessionID: session.ID}); err != nil {
		t.Fatal(err)
	}
	if sessions.tasks.Busy() {
		t.Fatal("ended MCP task retained")
	}
}

func TestSharedDaemonMCPSession(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	record := localShellRecord{Name: "daemon", Version: version, Token: resourceToken(), Address: listener.Addr().String()}
	tasks := &daemonTasks{}
	services := &unixDaemonServices{sessions: newTrackedMCPSessions(tasks, nil)}
	done := make(chan error, 1)
	go func() {
		defer listener.Close()
		defer services.Close()
		done <- serveDaemon(listener, record, services, tasks)
	}()
	call := func(req mcp.Request) mcp.Response {
		t.Helper()
		req.Version = version
		var reply mcp.Response
		if err := daemonJSON(context.Background(), record, "/mcp", req, &reply); err != nil || reply.Error != "" {
			t.Fatalf("MCP: %+v %v", reply, err)
		}
		return reply
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
	snapshot := tasks.adminSnapshot(record).Snapshot
	if _, err := daemonAdministrationAt(context.Background(), record, "shutdown", &snapshot); err != nil {
		t.Fatal(err)
	}
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
	sessions := newTrackedMCPSessions(&daemonTasks{})
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
	child, err := proc.ReadTask(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Signal(pid, child.Start, 9) }()
	if _, err = sessions.Close(ctx, mcp.SessionInput{SessionID: session.ID}); err != nil {
		t.Fatal(err)
	}
	if now, err := proc.ReadTask(pid); err == nil && now.Start == child.Start && now.State != "Z" {
		t.Fatal("child still running")
	}
}

func TestTrackedMCPCloseAfterBashExit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	sessions := newTrackedMCPSessions(&daemonTasks{})
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
	sessions := newTrackedMCPSessions(&daemonTasks{}, func() error { return errors.New("setup changed") })
	if _, err := sessions.Open(context.Background(), mcp.OpenInput{Host: "work"}); err == nil {
		t.Fatal("changed setup accepted")
	}
	list, err := sessions.List(context.Background(), mcp.OpenInput{Host: "work"})
	if err != nil || len(list.Sessions) != 0 {
		t.Fatalf("%+v %v", list, err)
	}
}

func TestMCPAgentReconnect(t *testing.T) {
	e := newEnv(t)
	t.Setenv("SSHORE_RUNTIME_DIR", e.dir)
	call := func(req mcp.Request) mcp.Response {
		t.Helper()
		cmd := exec.Command(e.bin, "mcp-agent", version)
		cmd.Env = e.env
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		ready, err := readMessage(output)
		if err != nil || ready.Type != "setup_ready" || ready.Action != "managed" {
			t.Fatalf("setup %+v: %v", ready, err)
		}
		req.Version = version
		if err = mcp.WriteRequest(input, req); err != nil {
			t.Fatal(err)
		}
		reply, err := mcp.ReadResponse(output)
		if err != nil || reply.Error != "" {
			t.Fatalf("reply %+v: %v", reply, err)
		}
		_ = input.Close()
		if err = cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	session := call(mcp.Request{Method: "open", Open: mcp.OpenInput{Host: "fixture"}}).Session
	snapshot, err := daemonAdmin(context.Background(), "snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, task := range snapshot.Tasks {
		if task.ID == session.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("MCP session absent from task table: %+v", snapshot.Tasks)
	}
	execute := func(code string) string {
		execution := call(mcp.Request{Method: "execute", Execute: mcp.ExecuteInput{SessionID: session.ID, Code: code}}).Execution
		deadline := time.Now().Add(5 * time.Second)
		cursor := ""
		output := ""
		for time.Now().Before(deadline) {
			result := call(mcp.Request{Method: "read", Read: mcp.ReadInput{SessionID: session.ID, ExecutionID: execution.ID, Cursor: cursor, WaitMS: 20}}).Output
			output += result.Stdout
			cursor = result.Cursor
			if result.State == "completed" && result.OutputClosed {
				return output
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("execution timeout")
		return ""
	}
	execute("value=persistent")
	if got := execute("printf '%s' \"$value\""); got != "persistent" {
		t.Fatalf("output %q", got)
	}
	call(mcp.Request{Method: "close", Session: mcp.SessionInput{SessionID: session.ID}})
}

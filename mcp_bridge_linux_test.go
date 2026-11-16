//go:build linux && sshoretest

package main

import (
	"github.com/jay-waves/sshore/internal/mcp"
	"os/exec"
	"testing"
	"time"
)

func TestMCPBootstrapReconnect(t *testing.T) {
	e := newEnv(t)
	t.Setenv("SSHORE_RUNTIME_DIR", e.dir)
	call := func(req mcp.Request) mcp.Response {
		t.Helper()
		cmd := exec.Command(e.bin, "bootstrap", e.bin, "mcp-agent", "")
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
	processes, err := findSessionDaemons("/proc")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, process := range processes {
		if process.name == "mcp-"+session.ID {
			found = true
			if health := probeSessionDaemon(process, time.Now().Add(time.Second)); health.state != "detached" {
				t.Fatalf("MCP daemon health: %+v", health)
			}
		}
	}
	if !found {
		t.Fatalf("MCP session absent from account scan: %+v", processes)
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

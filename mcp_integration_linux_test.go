//go:build linux && sshoretest

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	sshoremcp "github.com/jay-waves/sshore/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPStdioReconnect(t *testing.T) {
	e := newEnv(t)
	wrapper := filepath.Join(e.dir, "ssh-wrapper")
	script := "#!/bin/sh\nexec \"$SSHORE_TEST_REMOTE_BIN\" bootstrap \"$SSHORE_TEST_REMOTE_BIN\" mcp-agent ''\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
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
	var session sshoremcp.Session
	call(c, "session_open", map[string]any{"host": "test"}, &session)
	var execution sshoremcp.Execution
	call(c, "session_execute", map[string]any{"session_id": session.ID, "code": "x=retained"}, &execution)
	wait := func(client *sdk.ClientSession, executionID string) string {
		t.Helper()
		cursor := ""
		text := ""
		for {
			var result sshoremcp.Output
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
	var sessions sshoremcp.Sessions
	call(c, "session_list", map[string]any{"host": "test"}, &sessions)
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].ID != session.ID {
		t.Fatalf("sessions %+v", sessions)
	}
	call(c, "session_execute", map[string]any{"session_id": session.ID, "code": "printf '%s' \"$x\""}, &execution)
	if got := wait(c, execution.ID); got != "retained" {
		t.Fatalf("output %q", got)
	}
	var closed sshoremcp.Closed
	call(c, "session_close", map[string]any{"session_id": session.ID}, &closed)
	if !closed.Closed {
		t.Fatal("session not closed")
	}
}

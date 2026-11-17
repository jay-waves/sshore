package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type testBackend struct {
	execute ExecuteInput
	output  *Output
}

func (*testBackend) Open(_ context.Context, in OpenInput) (Session, error) {
	return Session{ID: "id", Host: in.Host, State: "ready"}, nil
}
func (*testBackend) List(context.Context, OpenInput) (Sessions, error) { return Sessions{}, nil }
func (b *testBackend) Execute(_ context.Context, in ExecuteInput) (Execution, error) {
	b.execute = in
	return Execution{ID: "run", State: "running"}, nil
}
func (b *testBackend) Read(context.Context, ReadInput) (Output, error) {
	if b.output != nil {
		return *b.output, nil
	}
	return Output{}, errors.New("execution missing")
}
func (*testBackend) Close(context.Context, SessionInput) (Closed, error) {
	return Closed{Closed: true}, nil
}

func TestToolsOverMCP(t *testing.T) {
	ctx := context.Background()
	b := new(testBackend)
	ct, st := sdk.NewInMemoryTransports()
	server, err := New("1.2.3", b).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools, err := client.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 5 {
		t.Fatalf("tools: %+v %v", tools, err)
	}
	result, err := client.CallTool(ctx, &sdk.CallToolParams{Name: "session_execute", Arguments: map[string]any{"session_id": "id", "code": "x=1\nprintf '%s' \"$x\"", "stdin": "input"}})
	if err != nil || result.IsError {
		t.Fatalf("execute: %+v %v", result, err)
	}
	if b.execute.SessionID != "id" || b.execute.Stdin != "input" || b.execute.Code != "x=1\nprintf '%s' \"$x\"" {
		t.Fatalf("arguments: %+v", b.execute)
	}
	result, err = client.CallTool(ctx, &sdk.CallToolParams{Name: "session_read", Arguments: map[string]any{"session_id": "id", "execution_id": "unknown"}})
	if err != nil || !result.IsError {
		t.Fatalf("backend error: %+v %v", result, err)
	}
	b.output = &Output{State: "completed", Stdout: "done", Cursor: "4:0", OutputClosed: true}
	result, err = client.CallTool(ctx, &sdk.CallToolParams{Name: "session_execute", Arguments: map[string]any{"session_id": "id", "code": "printf done", "wait_ms": 1000}})
	if err != nil || result.IsError {
		t.Fatalf("execute with output: %+v %v", result, err)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var execution Execution
	if err = json.Unmarshal(data, &execution); err != nil {
		t.Fatal(err)
	}
	if execution.ID != "run" || execution.State != "completed" || execution.Output == nil || execution.Output.Stdout != "done" || !execution.Output.OutputClosed {
		t.Fatalf("inconsistent execution result: %+v", execution)
	}

}

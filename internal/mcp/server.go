// Package mcp exposes the application's execution sessions as MCP tools.
package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type OpenInput struct {
	Host string `json:"host" jsonschema:"SSH host or configuration alias"`
}
type SessionInput struct {
	SessionID string `json:"session_id"`
}
type ExecuteInput struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code" jsonschema:"Complete Bash script to execute"`
	WaitMS    int    `json:"wait_ms,omitempty" jsonschema:"Wait up to 30000 ms for initial output; zero returns immediately"`
	Stdin     string `json:"stdin,omitempty" jsonschema:"Fixed standard input"`
}
type ReadInput struct {
	SessionID   string `json:"session_id"`
	ExecutionID string `json:"execution_id"`
	Cursor      string `json:"cursor,omitempty"`
	WaitMS      int    `json:"wait_ms,omitempty"`
}
type Session struct {
	ID    string `json:"id"`
	Host  string `json:"host"`
	State string `json:"state"`
}
type Sessions struct {
	Sessions []Session `json:"sessions"`
}
type Execution struct {
	Output    *Output `json:"output,omitempty"`
	ReadError string  `json:"read_error,omitempty"`
	ID        string  `json:"id"`
	State     string  `json:"state"`
}
type Output struct {
	Stdout       string `json:"stdout"`
	StdoutBase64 string `json:"stdout_base64,omitempty"`
	Stderr       string `json:"stderr"`
	StderrBase64 string `json:"stderr_base64,omitempty"`
	Cursor       string `json:"cursor"`
	State        string `json:"state"`
	Error        string `json:"error,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	OutputClosed bool   `json:"output_closed"`
	Truncated    bool   `json:"truncated"`
}
type Closed struct {
	Closed bool `json:"closed"`
}

// Backend owns session identities, execution, transport and lifecycle policy.
type Backend interface {
	Open(context.Context, OpenInput) (Session, error)
	List(context.Context, OpenInput) (Sessions, error)
	Execute(context.Context, ExecuteInput) (Execution, error)
	Read(context.Context, ReadInput) (Output, error)
	Close(context.Context, SessionInput) (Closed, error)
}

func New(version string, b Backend) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "sshore", Version: version}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "session_open", Description: "Open a persistent remote Bash session without a PTY."}, func(ctx context.Context, _ *sdk.CallToolRequest, in OpenInput) (*sdk.CallToolResult, Session, error) {
		if err := validateHost(in.Host); err != nil {
			return nil, Session{}, err
		}
		out, err := b.Open(ctx, in)
		return nil, out, err
	})
	sdk.AddTool(s, &sdk.Tool{Name: "session_list", Description: "List remote Bash sessions for an SSH host."}, func(ctx context.Context, _ *sdk.CallToolRequest, in OpenInput) (*sdk.CallToolResult, Sessions, error) {
		if err := validateHost(in.Host); err != nil {
			return nil, Sessions{}, err
		}
		out, err := b.List(ctx, in)
		return nil, out, err
	})
	sdk.AddTool(s, &sdk.Tool{Name: "session_execute", Description: "Submit a complete Bash script. State persists in the session. Only one script can run at a time; a busy session rejects another submission. Returns an execution ID. Set wait_ms to include initial output; continue session_read with its cursor."}, func(ctx context.Context, _ *sdk.CallToolRequest, in ExecuteInput) (*sdk.CallToolResult, Execution, error) {
		if in.SessionID == "" || strings.IndexByte(in.Code, 0) >= 0 || in.WaitMS < 0 || in.WaitMS > 30000 {
			return nil, Execution{}, fmt.Errorf("invalid session or NUL in code")
		}
		out, err := b.Execute(ctx, in)
		if err == nil && in.WaitMS > 0 {
			output, readErr := b.Read(ctx, ReadInput{SessionID: in.SessionID, ExecutionID: out.ID, WaitMS: in.WaitMS})
			if readErr != nil {
				out.ReadError = readErr.Error()
			} else {
				out.Output = &output
				out.State = output.State
			}
		}
		return nil, out, err
	})
	sdk.AddTool(s, &sdk.Tool{Name: "session_read", Description: "Read bounded output from an execution. Reuse the returned cursor to continue. stdout_base64 or stderr_base64 contains bytes when a chunk is not UTF-8. Completion and output_closed are separate; background tasks can keep streams open. Old completed executions may be evicted."}, func(ctx context.Context, _ *sdk.CallToolRequest, in ReadInput) (*sdk.CallToolResult, Output, error) {
		if in.SessionID == "" || in.ExecutionID == "" || in.WaitMS < 0 || in.WaitMS > 30000 {
			return nil, Output{}, fmt.Errorf("invalid session, execution or wait")
		}
		out, err := b.Read(ctx, in)
		return nil, out, err
	})
	sdk.AddTool(s, &sdk.Tool{Name: "session_close", Description: "Close the session and end its identified same-user tasks."}, func(ctx context.Context, _ *sdk.CallToolRequest, in SessionInput) (*sdk.CallToolResult, Closed, error) {
		if in.SessionID == "" {
			return nil, Closed{}, fmt.Errorf("session ID required")
		}
		out, err := b.Close(ctx, in)
		return nil, out, err
	})
	return s
}

func validateHost(host string) error {
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, "\x00\r\n") {
		return fmt.Errorf("invalid SSH host")
	}
	return nil
}

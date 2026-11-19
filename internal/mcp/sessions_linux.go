//go:build linux

package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *BashSession) State() string {
	if channelClosed(s.finished) {
		return "exited"
	}
	if s.unavailable.Load() {
		return "unavailable"
	}
	return "ready"
}

func (s *BashSession) Execute(ctx context.Context, in ExecuteInput) (Execution, error) {
	execution, err := s.run(ctx, in.Code, in.Stdin)
	if err != nil {
		return Execution{}, err
	}
	return Execution{ID: execution.ID, State: "running"}, nil
}

func (s *BashSession) Read(ctx context.Context, in ReadInput) (Output, error) {
	s.mu.Lock()
	index := slices.IndexFunc(s.executions, func(execution *bashExecution) bool { return execution.ID == in.ExecutionID })
	if index < 0 {
		s.mu.Unlock()
		return Output{}, fmt.Errorf("execution not found")
	}
	e := s.executions[index]
	s.mu.Unlock()
	outPos, errPos, err := parseCursor(in.Cursor)
	if err != nil {
		return Output{}, err
	}
	const chunkSize = 4096
	result, err := e.readOutput(ctx, outPos, errPos, chunkSize, time.Duration(in.WaitMS)*time.Millisecond)
	if err != nil {
		return Output{}, err
	}
	state := "running"
	errorText := ""
	if result.Err != nil {
		state = "unknown"
		errorText = result.Err.Error()
	}
	var code *int
	if result.Complete {
		state = "completed"
		n := result.ExitCode
		code = &n
	}
	stdout, stdout64 := outputChunk(result.Stdout)
	stderr, stderr64 := outputChunk(result.Stderr)
	return Output{Stdout: stdout, StdoutBase64: stdout64, Stderr: stderr, StderrBase64: stderr64, Cursor: fmt.Sprintf("%d:%d", outPos+len(result.Stdout), errPos+len(result.Stderr)), State: state, Error: errorText, ExitCode: code, OutputClosed: result.OutputClosed, Truncated: result.Truncated}, nil
}
func parseCursor(cursor string) (int, int, error) {
	if cursor == "" {
		return 0, 0, nil
	}
	a, b, ok := strings.Cut(cursor, ":")
	x, e1 := strconv.Atoi(a)
	y, e2 := strconv.Atoi(b)
	if !ok || e1 != nil || e2 != nil || x < 0 || y < 0 {
		return 0, 0, fmt.Errorf("invalid output cursor")
	}
	return x, y, nil
}
func outputChunk(data []byte) (string, string) {
	if utf8.Valid(data) {
		return string(data), ""
	}
	return "", base64.StdEncoding.EncodeToString(data)
}

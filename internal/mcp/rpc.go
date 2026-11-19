package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// Request and Response are exchanged over the authenticated SSH transport.
type Request struct {
	Version string       `json:"version"`
	Method  string       `json:"method"`
	Open    OpenInput    `json:"open,omitzero"`
	Session SessionInput `json:"session,omitzero"`
	Execute ExecuteInput `json:"execute,omitzero"`
	Read    ReadInput    `json:"read,omitzero"`
}
type Response struct {
	Version   string     `json:"version"`
	Error     string     `json:"error,omitempty"`
	Session   *Session   `json:"session,omitempty"`
	Sessions  *Sessions  `json:"sessions,omitempty"`
	Execution *Execution `json:"execution,omitempty"`
	Output    *Output    `json:"output,omitempty"`
	Closed    *Closed    `json:"closed,omitempty"`
}

// Dispatch validates the exact release before invoking an operation.
func Dispatch(ctx context.Context, b Backend, req Request, version string) Response {
	res := Response{Version: version}
	if req.Version != version {
		res.Error = "release version mismatch"
		return res
	}
	var err error
	switch req.Method {
	case "open":
		var v Session
		v, err = b.Open(ctx, req.Open)
		res.Session = &v
	case "list":
		var v Sessions
		v, err = b.List(ctx, req.Open)
		res.Sessions = &v
	case "execute":
		var v Execution
		v, err = b.Execute(ctx, req.Execute)
		res.Execution = &v
	case "read":
		var v Output
		v, err = b.Read(ctx, req.Read)
		res.Output = &v
	case "close":
		var v Closed
		v, err = b.Close(ctx, req.Session)
		res.Closed = &v
	default:
		err = fmt.Errorf("unknown operation")
	}
	if err != nil {
		return Response{Version: version, Error: err.Error()}
	}
	return res
}

func WriteRequest(w io.Writer, req Request) error {
	return writeRPCFrame(w, req)
}
func ReadRequest(r io.Reader) (Request, error) {
	var req Request
	err := readRPCFrame(r, &req)
	return req, err
}
func WriteResponse(w io.Writer, res Response) error {
	return writeRPCFrame(w, res)
}
func ReadResponse(r io.Reader) (Response, error) {
	var res Response
	err := readRPCFrame(r, &res)
	return res, err
}

func writeRPCFrame(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeRPCPayload(w, data)
}
func readRPCFrame(r io.Reader, value any) error {
	data, err := readRPCPayload(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

const MaxRPCPayload = 64*1024 + 4096

func writeRPCPayload(w io.Writer, data []byte) error {
	if len(data) > MaxRPCPayload {
		return fmt.Errorf("MCP message exceeds %d bytes", MaxRPCPayload)
	}
	line := append(append(make([]byte, 0, len(data)+1), data...), '\n')
	for len(line) != 0 {
		n, err := w.Write(line)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		line = line[n:]
	}
	return nil
}

func readRPCPayload(r io.Reader) ([]byte, error) {
	reader, ok := r.(io.ByteReader)
	if !ok {
		reader = rpcByteReader{r}
	}
	var data []byte
	for {
		b, err := reader.ReadByte()
		if err != nil {
			if err == io.EOF && len(data) != 0 {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if b == '\n' {
			return data, nil
		}
		if len(data) == MaxRPCPayload {
			return nil, fmt.Errorf("MCP message exceeds %d bytes", MaxRPCPayload)
		}
		data = append(data, b)
	}
}

type rpcByteReader struct{ io.Reader }

func (r rpcByteReader) ReadByte() (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r.Reader, b[:])
	return b[0], err
}

// Relay passes one bounded JSON exchange at a time to the authenticated daemon.
// The daemon decodes requests and owns release checks and dispatch.
func Relay(r io.Reader, w io.Writer, exchange func(json.RawMessage) (json.RawMessage, error)) error {
	reader := bufio.NewReader(r)
	for {
		request, err := readRPCPayload(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		response, err := exchange(request)
		if err != nil {
			return err
		}
		if err := writeRPCPayload(w, response); err != nil {
			return err
		}
	}
}

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jay-waves/sshore/internal/wire"
)

// Request and Response are exchanged over the authenticated SSH transport.
type Request struct {
	Version string       `json:"version"`
	Method  string       `json:"method"`
	Open    OpenInput    `json:"open,omitempty"`
	Session SessionInput `json:"session,omitempty"`
	Execute ExecuteInput `json:"execute,omitempty"`
	Read    ReadInput    `json:"read,omitempty"`
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

// Dispatch invokes one operation after the caller has validated the release.
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
		res.Error = err.Error()
		res.Session = nil
		res.Sessions = nil
		res.Execution = nil
		res.Output = nil
		res.Closed = nil
	}
	return res
}

func WriteRequest(w io.Writer, req Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return wire.WriteFrame(w, 0, data)
}
func ReadRequest(r io.Reader) (Request, error) {
	var req Request
	_, data, err := wire.ReadFrame(r)
	if err != nil {
		return req, err
	}
	err = json.Unmarshal(data, &req)
	return req, err
}
func WriteResponse(w io.Writer, res Response) error {
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return wire.WriteFrame(w, 0, data)
}
func ReadResponse(r io.Reader) (Response, error) {
	var res Response
	_, data, err := wire.ReadFrame(r)
	if err != nil {
		return res, err
	}
	err = json.Unmarshal(data, &res)
	return res, err
}

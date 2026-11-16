package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

type DialFunc func(context.Context, string) (io.ReadWriteCloser, error)

// Client routes each request to its host; session references survive client restarts.
type Client struct {
	Version string
	Dial    DialFunc
}
type sessionReference struct {
	Host string `json:"host"`
	ID   string `json:"id"`
}

func encodeReference(host, id string) (string, error) {
	if err := validateHost(host); err != nil {
		return "", err
	}
	if id == "" || len(id) > 256 {
		return "", fmt.Errorf("invalid session ID")
	}
	b, err := json.Marshal(sessionReference{Host: host, ID: id})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func decodeReference(id string) (sessionReference, error) {
	var ref sessionReference
	if len(id) > 2048 {
		return ref, fmt.Errorf("invalid session reference")
	}
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return ref, fmt.Errorf("invalid session reference")
	}
	if err = json.Unmarshal(b, &ref); err != nil || ref.ID == "" || len(ref.ID) > 256 || validateHost(ref.Host) != nil {
		return sessionReference{}, fmt.Errorf("invalid session reference")
	}
	return ref, nil
}
func (c *Client) call(ctx context.Context, host string, req Request) (Response, error) {
	if err := validateHost(host); err != nil {
		return Response{}, err
	}
	if c.Dial == nil {
		return Response{}, fmt.Errorf("transport unavailable")
	}
	conn, err := c.Dial(ctx, host)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	req.Version = c.Version
	if err = WriteRequest(conn, req); err != nil {
		return Response{}, fmt.Errorf("send request: %w", err)
	}
	res, err := ReadResponse(conn)
	if err != nil {
		return Response{}, fmt.Errorf("response unavailable; operation outcome may be unknown: %w", err)
	}
	if res.Version != c.Version {
		return Response{}, fmt.Errorf("remote release mismatch")
	}
	if res.Error != "" {
		return Response{}, fmt.Errorf("remote: %s", res.Error)
	}
	return res, nil
}
func (c *Client) Open(ctx context.Context, in OpenInput) (Session, error) {
	r, err := c.call(ctx, in.Host, Request{Method: "open", Open: in})
	if err != nil {
		return Session{}, err
	}
	if r.Session == nil {
		return Session{}, fmt.Errorf("invalid response")
	}
	s := *r.Session
	s.ID, err = encodeReference(in.Host, s.ID)
	s.Host = in.Host
	return s, err
}
func (c *Client) List(ctx context.Context, in OpenInput) (Sessions, error) {
	r, err := c.call(ctx, in.Host, Request{Method: "list", Open: in})
	if err != nil {
		return Sessions{}, err
	}
	if r.Sessions == nil {
		return Sessions{}, fmt.Errorf("invalid response")
	}
	for i := range r.Sessions.Sessions {
		s := &r.Sessions.Sessions[i]
		s.ID, err = encodeReference(in.Host, s.ID)
		if err != nil {
			return Sessions{}, err
		}
		s.Host = in.Host
	}
	return *r.Sessions, nil
}
func (c *Client) Execute(ctx context.Context, in ExecuteInput) (Execution, error) {
	ref, err := decodeReference(in.SessionID)
	if err != nil {
		return Execution{}, err
	}
	in.SessionID = ref.ID
	r, err := c.call(ctx, ref.Host, Request{Method: "execute", Execute: in})
	if err != nil {
		return Execution{}, err
	}
	if r.Execution == nil {
		return Execution{}, fmt.Errorf("invalid response")
	}
	return *r.Execution, nil
}
func (c *Client) Read(ctx context.Context, in ReadInput) (Output, error) {
	ref, err := decodeReference(in.SessionID)
	if err != nil {
		return Output{}, err
	}
	in.SessionID = ref.ID
	r, err := c.call(ctx, ref.Host, Request{Method: "read", Read: in})
	if err != nil {
		return Output{}, err
	}
	if r.Output == nil {
		return Output{}, fmt.Errorf("invalid response")
	}
	return *r.Output, nil
}
func (c *Client) Close(ctx context.Context, in SessionInput) (Closed, error) {
	ref, err := decodeReference(in.SessionID)
	if err != nil {
		return Closed{}, err
	}
	in.SessionID = ref.ID
	r, err := c.call(ctx, ref.Host, Request{Method: "close", Session: in})
	if err != nil {
		return Closed{}, err
	}
	if r.Closed == nil {
		return Closed{}, fmt.Errorf("invalid response")
	}
	return *r.Closed, nil
}

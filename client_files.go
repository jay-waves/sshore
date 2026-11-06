package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/xtaci/smux"
)

type clientTransfer struct {
	id        uint64
	stream    *smux.Stream
	request   message
	ctx       context.Context
	cancel    context.CancelFunc
	progress  atomic.Int64
	total     atomic.Int64
	closeOnce sync.Once
}

func (t *clientTransfer) close() {
	t.cancel()
	// Close wakes blocked reads/writes immediately, but sending FIN may wait for
	// the transport. Do not perform that potentially slow operation in the UI.
	t.closeOnce.Do(func() { go func() { _ = t.stream.Close() }() })
}

type fileReply struct {
	m   message
	err error
}

// Each approved task owns its files, exact local path and one smux stream.
// Closing the stream cancels this task without affecting sibling streams.
func runFileTask(t *clientTransfer, path string) (result string, failure error) {
	defer t.close()
	defer func() {
		if failure != nil && t.ctx.Err() == nil {
			_ = streamMessage(t.stream, message{Type: "error", ID: t.id, Error: failure.Error()})
		}
	}()
	inbox := make(chan fileReply, 2)
	go func() {
		defer close(inbox)
		for {
			m, err := readMessage(t.stream)
			select {
			case inbox <- fileReply{m, err}:
			case <-t.ctx.Done():
				return
			}
			if err != nil || m.Type == "error" || m.Type == "cancel" || m.Type == "fatal" {
				// A rejected upload must wake a writer blocked by backpressure.
				_ = t.stream.Close()
				return
			}
		}
	}()
	next := func() (message, error) {
		if err := t.ctx.Err(); err != nil {
			return message{}, err
		}
		select {
		case <-t.ctx.Done():
			return message{}, t.ctx.Err()
		case r, ok := <-inbox:
			if !ok {
				return message{}, io.EOF
			}
			if r.err != nil {
				return message{}, r.err
			}
			if r.m.ID != t.id {
				return message{}, fmt.Errorf("wrong transfer ID")
			}
			return r.m, nil
		}
	}
	if t.request.Type == "upload" {
		f, err := openSending(path)
		if err != nil {
			return "", err
		}
		defer f.close()
		if t.request.Name != "" && (f.name != t.request.Name || f.size != t.request.Size) {
			return "", fmt.Errorf("local file metadata changed; request again")
		}
		t.total.Store(f.size)
		if err = writeMessage(t.stream, f.offer(t.id)); err != nil {
			return "", err
		}
		m, err := next()
		if err != nil {
			return "", err
		}
		if m.Type != "accept" {
			return "", fileResponseError(m)
		}
		for {
			if err = t.ctx.Err(); err != nil {
				return "", err
			}
			m, err = f.next(t.id)
			if err != nil {
				return "", err
			}
			if err = writeMessage(t.stream, m); err != nil {
				return "", err
			}
			t.progress.Store(f.done)
			if m.Type == "end" {
				break
			}
		}
		m, err = next()
		if err != nil {
			return "", err
		}
		if m.Type != "result" || m.Error != "" {
			return "", fileResponseError(m)
		}
		return "Upload complete", nil
	}
	f, err := newReceiving(path, t.request.Size)
	if err != nil {
		return "", err
	}
	defer f.close()
	if err = writeMessage(t.stream, message{Type: "accept", ID: t.id}); err != nil {
		return "", err
	}
	for {
		m, err := next()
		if err != nil {
			return "", err
		}
		switch m.Type {
		case "chunk":
			if err = f.put(m.Data); err != nil {
				return "", err
			}
			t.progress.Store(f.done)
		case "end":
			if err = t.ctx.Err(); err != nil {
				return "", err
			}
			err = f.finish(m.Hash)
			result := message{Type: "result", ID: t.id}
			if err != nil {
				result.Error = err.Error()
			}
			if e := writeMessage(t.stream, result); e != nil {
				return "", e
			}
			if err != nil {
				return "", err
			}
			return "Saved " + path, nil
		default:
			return "", fileResponseError(m)
		}
	}
}
func fileResponseError(m message) error {
	if m.Error != "" {
		return fmt.Errorf("%s", m.Error)
	}
	return fmt.Errorf("transfer cancelled or unexpected response: %s", m.Type)
}

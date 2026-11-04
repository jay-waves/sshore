package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type fileReply struct {
	m   message
	err error
}

// Each task owns its file handles and one smux stream. All disk and transfer I/O
// stays out of the terminal event loop. Closing the stream cancels just this task.
func runFileTask(t *clientTransfer) (result string, failure error) {
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
			if err != nil {
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
	var path string
	select {
	case <-t.ctx.Done():
		return "", t.ctx.Err()
	case r := <-inbox:
		if r.err != nil {
			return "", r.err
		}
		return "", fmt.Errorf("unexpected message before file selection: %s", r.m.Type)
	case path = <-t.choice:
	}
	if t.request.Type == "upload" {
		f, err := openSending(path)
		if err != nil {
			return "", err
		}
		defer f.close()
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
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, t.request.Name)
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

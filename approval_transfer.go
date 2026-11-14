package main

// Approved files execute sequentially; the first failure stops the batch without
// rolling back completed files. Ctrl-C cancels local file operations but leaves
// the remote shell alive. Normal terminal output is muted until --attach resumes
// it. --stay returns after file operations. Approved forwarding is handled by
// the client as an independent background resource after detaching the shell.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/xtaci/smux"
)

func executeApprovedFiles(ctx context.Context, mux *smux.Session, ops []operation, quiet bool) error {
	for _, op := range ops {
		if op.Kind == "forward" {
			continue
		}
		if err := executeApprovedFile(ctx, mux, op, quiet); err != nil {
			return err
		}
	}
	return nil
}
func executeApprovedFile(ctx context.Context, mux *smux.Session, op operation, quiet bool) error {
	st, err := mux.OpenStream()
	if err != nil {
		return err
	}
	defer st.Close()
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	remote := op
	remote.LocalPath = ""
	if remote.Kind == "upload" {
		remote.Name = ""
		remote.Size = 0
	}
	if err = writeMessage(st, message{Type: "approved_transfer", Operations: []operation{remote}}); err != nil {
		return err
	}
	m, err := readMessage(st)
	if err != nil {
		return err
	}
	if m.Type == "error" || m.Type == "fatal" {
		return fileResponseError(m)
	}
	if op.Kind == "download" {
		if m.Type != "offer" || m.Name != op.Name || m.Size != op.Size || m.ID != 0 {
			return fmt.Errorf("remote download does not match approval")
		}
	} else {
		if m.Type != "upload" || m.ID != 0 {
			return fmt.Errorf("unexpected upload response")
		}
		m.Name = op.Name
		m.Size = op.Size
	}
	_ = st.SetDeadline(time.Time{})
	taskCtx, cancel := context.WithCancel(ctx)
	t := &clientTransfer{id: 0, stream: st, request: m, ctx: taskCtx, cancel: cancel}
	t.total.Store(op.Size)
	done := make(chan struct {
		text string
		err  error
	}, 1)
	go func() {
		text, err := runFileTask(t, op.LocalPath)
		done <- struct {
			text string
			err  error
		}{text, err}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case r := <-done:
			if r.err != nil {
				return r.err
			}
			logf(os.Stderr, logSuccess, "[sshore] %s\n", cleanText(r.text))
			return nil
		case <-ticker.C:
			if !quiet {
				logf(os.Stderr, logInfo, "[sshore] %s: %d/%d bytes\n", op.Kind, t.progress.Load(), t.total.Load())
			}
		}
	}
}

package main

// Approved files execute sequentially; the first failure stops the batch without
// rolling back completed files. Ctrl-C cancels local file operations but leaves
// the remote shell alive. Normal terminal output is muted until --attach resumes
// it. --stay returns after file operations, but an approved port keeps a foreground
// transport running until cancellation. There is no background tunnel service.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
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

func stayWithForwards(mux *smux.Session, control, terminal *smux.Stream, forwards *forwardManager) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Read Ctrl-C as a local key, not a terminal-generated process-group signal:
	// otherwise it can kill OpenSSH before this process handles cancellation.
	restore, err := rawTerminal()
	if err != nil {
		return 1, err
	}
	defer func() { restore(); _ = flushTerminalInput() }()
	logf(os.Stderr, logInfo, "[sshore] Forwarding is active. Staying local; Ctrl-C closes forwarding and the remote session.\r\n")

	done := make(chan error, 4)
	go func() {
		input := terminalInputReader()
		buf := make([]byte, 4096)
		for {
			n, err := input.Read(buf)
			if bytes.IndexByte(buf[:n], 3) >= 0 {
				done <- nil
				return
			}
			if err != nil {
				done <- err
				return
			}
			// All other local input is ignored, never forwarded to the remote shell.
		}
	}()
	// Do not render remote terminal output or accept new sensitive operations in
	// local mode. Drain terminal output so it cannot block the remote shell.
	go func() {
		for {
			m, e := readMessage(terminal)
			if e != nil {
				done <- e
				return
			}
			if m.Type == "exit" {
				done <- nil
				return
			}
		}
	}()
	go func() {
		for {
			m, e := readMessage(control)
			if e != nil {
				done <- e
				return
			}
			if m.Type == "fatal" {
				done <- fileResponseError(m)
				return
			}
			if m.Type == "detach_requested" {
				if e := streamMessage(control, message{Type: "detach"}); e != nil {
					done <- e
					return
				}
				continue
			}
			if m.Type == "detached" {
				_ = streamMessage(control, message{Type: "bye"})
				done <- nil
				return
			}
		}
	}()
	go func() {
		for {
			st, e := mux.AcceptStream()
			if e != nil {
				done <- e
				return
			}
			_ = st.SetReadDeadline(time.Now().Add(forwardTimeout))
			m, e := readMessage(st)
			_ = st.SetReadDeadline(time.Time{})
			if e == nil && m.Type == "forward_connect" && m.Reverse {
				go forwards.handleReverse(st, m)
			} else {
				_ = st.Close()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return 0, nil
	case err := <-done:
		return 0, err
	}
}

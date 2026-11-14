package main

// Approved files execute sequentially; the first failure stops the batch without
// rolling back completed files. Ctrl-C cancels local file operations but leaves
// the remote shell alive. Normal terminal output is muted until attachment resumes
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
	var files []operation
	for _, op := range ops {
		if op.Kind != "forward" {
			files = append(files, op)
		}
	}
	for i, op := range files {
		if !quiet {
			logf(os.Stderr, logInfo, "[sshore] File %d/%d: %s %s\n", i+1, len(files), op.Kind, cleanText(op.Name))
		}
		if err := executeApprovedFile(ctx, mux, op, quiet); err != nil {
			logf(os.Stderr, logError, "[sshore] Transfer summary: %d completed, 1 failed/cancelled, %d not started. Completed files are retained.\n", i, len(files)-i-1)
			for j, file := range files {
				state := "not started"
				if j < i {
					state = "completed"
				} else if j == i {
					state = "failed/cancelled"
				}
				logf(os.Stderr, logInfo, "[sshore] %s: %s (%s)\n", state, cleanText(file.Name), cleanText(file.LocalPath))
			}
			logf(os.Stderr, logInfo, "[sshore] Incomplete downloads are removed when cleanup succeeds; forced termination can leave .sshore-*.part files. Remote upload cleanup is best effort.\n")
			return err
		}
	}
	if len(files) > 0 {
		logf(os.Stderr, logSuccess, "[sshore] Transfer summary: %d/%d files completed.\n", len(files), len(files))
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
	var progress transferProgress
	progress.at = time.Now()
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
				logf(os.Stderr, logInfo, "[sshore] %s %s: %s\n", op.Kind, cleanText(op.Name), progress.sample(time.Now(), t.progress.Load(), t.total.Load()))
			}
		}
	}
}

// Rate is a time-smoothed estimate, never a promise of remote persistence.
type transferProgress struct {
	at    time.Time
	bytes int64
	rate  float64
}

func (p *transferProgress) sample(now time.Time, done, total int64) string {
	elapsed := now.Sub(p.at).Seconds()
	if elapsed > 0 {
		rate := float64(max(int64(0), done-p.bytes)) / elapsed
		if p.at.IsZero() {
			rate = 0
		}
		if p.rate == 0 {
			p.rate = rate
		} else {
			p.rate = .3*rate + .7*p.rate
		}
	}
	p.at, p.bytes = now, done
	eta := "unknown"
	if done >= total {
		eta = "0s"
	} else if p.rate >= 1 {
		seconds := float64(total-done) / p.rate
		if seconds < float64((1<<63-1)/int64(time.Second)) {
			eta = (time.Duration(seconds) * time.Second).String()
		}
	}
	return fmt.Sprintf("%s/%s · %s/s · remaining %s", displayBytes(done), displayBytes(total), displayBytes(int64(p.rate)), eta)
}

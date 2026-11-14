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
	"sync/atomic"
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
	if op.Kind == "upload" {
		f, err := openSending(op.LocalPath)
		if err != nil {
			return err
		}
		f.close()
		if f.name != op.Name || f.size != op.Size {
			return fmt.Errorf("local file metadata changed; request again")
		}
		remote.Name, remote.Size = "", 0
	}
	if err = writeMessage(st, message{Type: "approved_transfer", Operations: []operation{remote}, Name: op.Name, Size: op.Size}); err != nil {
		return err
	}
	m, err := readMessage(st)
	if err != nil {
		return err
	}
	if m.Type != "sftp" {
		return fileResponseError(m)
	}
	client, err := newFTPSFTP(st, st)
	if err != nil {
		return err
	}
	defer func() { st.Close(); client.Close() }()
	_ = st.SetDeadline(time.Time{})
	var completed atomic.Int64
	done := make(chan error, 1)
	go func() {
		if op.Kind == "upload" {
			done <- copyUpload(client, op.LocalPath, "/target", op.Size, &completed)
		} else {
			done <- copyDownload(client, "/source", op.LocalPath, op.Size, &completed)
		}
	}()
	var progress transferProgress
	progress.at = time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				return err
			}
			result := "Saved " + op.LocalPath
			if op.Kind == "upload" {
				result = "Upload complete"
			}
			logf(os.Stderr, logSuccess, "[sshore] %s\n", cleanText(result))
			return nil
		case <-ticker.C:
			if !quiet {
				logf(os.Stderr, logInfo, "[sshore] %s %s: %s\n", op.Kind, cleanText(op.Name), progress.sample(time.Now(), completed.Load(), op.Size))
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

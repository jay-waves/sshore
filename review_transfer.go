package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"

	"github.com/jay-waves/sshore/internal/files"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

// Approved files execute sequentially; the first failure stops the batch without
// rolling back completed files. Ctrl-C cancels local file operations; a failed
// transfer closes the attachment while preserving its shell.
// Normal terminal output is muted until attachment resumes
// it. --stay returns after file operations. Approved forwarding runs separately
// as an independent background resource without attaching to the shell.

func executeApprovedFiles(ctx context.Context, mux *smux.Session, ops []operation, quiet bool) error {
	var files []operation
	for _, op := range ops {
		if op.Kind != "forward" {
			files = append(files, op)
		}
	}
	for i, op := range files {
		if !quiet {
			termui.Log(os.Stderr, termui.Info, "[sshore] File %d/%d: %s %s\n", i+1, len(files), op.Kind, cleanText(op.Name))
		}
		if err := executeApprovedFile(ctx, mux, op, quiet); err != nil {
			termui.Log(os.Stderr, termui.Error, "[sshore] Transfer: %d done, 1 failed/cancelled, %d not started. Completed files kept.\n", i, len(files)-i-1)
			for j, file := range files {
				state := "not started"
				if j < i {
					state = "completed"
				} else if j == i {
					state = "failed/cancelled"
				}
				termui.Log(os.Stderr, termui.Info, "[sshore] %s: %s (%s)\n", state, cleanText(file.Name), cleanText(file.LocalPath))
			}
			termui.Log(os.Stderr, termui.Info, "[sshore] Cleanup may leave .sshore-*.part downloads or partial uploads.\n")
			return err
		}
	}
	if len(files) > 0 {
		termui.Log(os.Stderr, termui.Success, "[sshore] Transferred %d/%d files.\n", len(files), len(files))
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
		f, err := files.OpenSource(op.LocalPath)
		if err != nil {
			return err
		}
		f.Close()
		if f.Name() != op.Name || f.Size() != op.Size {
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
	client, err := files.NewSFTP(st, st)
	if err != nil {
		return err
	}
	defer func() { st.Close(); client.Close() }()
	_ = st.SetDeadline(time.Time{})
	var completed atomic.Int64
	done := make(chan error, 1)
	go func() {
		if op.Kind == "upload" {
			done <- files.Upload(client, op.LocalPath, "/target", op.Size, &completed)
		} else {
			done <- files.Download(client, "/source", op.LocalPath, op.Size, &completed)
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
			termui.Log(os.Stderr, termui.Success, "[sshore] %s\n", cleanText(result))
			return nil
		case <-ticker.C:
			if !quiet {
				termui.Log(os.Stderr, termui.Info, "[sshore] %s %s: %s\n", op.Kind, cleanText(op.Name), progress.sample(time.Now(), completed.Load(), op.Size))
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

// A shell attachment accepts only client-initiated approved file streams.
// Forwarding has its own SSH transport; obsolete tunnel/control streams cannot
// create listeners or dial services through this connection.

const maxApprovedStreams = sshio.MaxFileStreams

func serveApprovedStreams(ctx context.Context, mux *smux.Session) {
	slots := make(chan struct{}, maxApprovedStreams)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		st, e := mux.AcceptStream()
		if e != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			st.Close()
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-slots; st.Close() }()
			abort := context.AfterFunc(ctx, func() { st.SetDeadline(time.Now()); st.Close() })
			defer abort()
			st.SetDeadline(time.Now().Add(requestTimeout))
			m, e := readMessage(st)
			if e == nil && m.Type == "approved_transfer" {
				approvedRemoteTransfer(st, m)
			}
		}()
	}
}

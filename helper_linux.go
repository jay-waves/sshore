package main

// Remote shell helpers use SSHORE_SOCKET to request detach or transfer work from
// the session daemon. File bytes travel through this socket and a dedicated smux
// stream, never through the PTY. Multi-file download uses two workers and reports
// aggregate failure without canceling the remaining files; batches share the
// same two attachment-wide transfer slots as independent helper commands.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
)

func helper(action, path string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return helperContext(ctx, action, path)
}
func helperContext(ctx context.Context, action, path string) error {
	sock := os.Getenv("SSHORE_SOCKET")
	if sock == "" {
		return fmt.Errorf("run this command inside an sshore session")
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer c.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-done:
		}
	}()
	if action == "detach" {
		if err := writeMessage(c, message{Type: "detach"}); err != nil {
			return err
		}
		m, err := readMessage(c)
		if err != nil {
			return err
		}
		if m.Type != "detached" {
			return responseError(m)
		}
		return nil
	}
	var id uint64
	defer func() {
		if err != nil {
			_ = writeMessage(c, message{Type: "error", ID: id, Error: err.Error()})
		}
	}()
	if action == "download" {
		err = helperDownload(c, path, &id)
	} else {
		err = helperUpload(c, path, &id)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("transfer cancelled")
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return fmt.Errorf("transfer interrupted (cancelled or disconnected)")
	}
	return err
}
func responseError(m message) error {
	if m.Error != "" {
		return fmt.Errorf("%s", m.Error)
	}
	return fmt.Errorf("transfer cancelled or unexpected response: %s", m.Type)
}
func helperDownload(c net.Conn, path string, id *uint64) error {
	f, err := openSending(path)
	if err != nil {
		return err
	}
	defer f.close()
	if err = writeMessage(c, f.offer(0)); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	*id = m.ID
	if m.Type != "accept" {
		return responseError(m)
	}
	// Read cancellation concurrently, interrupting writes even under backpressure.
	result := make(chan error, 1)
	go func() {
		m, err := readMessage(c)
		if err == nil && (m.Type != "result" || m.Error != "") {
			err = responseError(m)
		}
		result <- err
		_ = c.Close()
	}()
	for {
		m, err := f.next(*id)
		if err != nil {
			return err
		}
		if err = writeMessage(c, m); err != nil {
			select {
			case e := <-result:
				if e != nil {
					return e
				}
			default:
			}
			return err
		}
		if m.Type == "end" {
			break
		}
	}
	if err := <-result; err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Downloaded %s (%d bytes)\n", f.name, f.size)
	return nil
}
func helperUpload(c net.Conn, dir string, id *uint64) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("upload destination must be a directory")
	}
	if err = writeMessage(c, message{Type: "upload"}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	*id = m.ID
	if m.Type != "offer" {
		return responseError(m)
	}
	if err = safeName(m.Name); err != nil {
		return err
	}
	f, err := newReceiving(filepath.Join(dir, m.Name), m.Size)
	if err != nil {
		return err
	}
	defer f.close()
	if err = writeMessage(c, message{Type: "accept", ID: *id}); err != nil {
		return err
	}
	for {
		m, err = readMessage(c)
		if err != nil {
			return err
		}
		if m.ID != *id {
			return fmt.Errorf("wrong transfer ID")
		}
		switch m.Type {
		case "chunk":
			if err = f.put(m.Data); err != nil {
				return err
			}
		case "end":
			err = f.finish(m.Hash)
			result := message{Type: "result", ID: *id}
			if err != nil {
				result.Error = err.Error()
			}
			if e := writeMessage(c, result); e != nil {
				return e
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Uploaded %s (%d bytes)\n", f.target, f.size)
			return nil
		default:
			return responseError(m)
		}
	}
}

// A small fixed worker pool batches files without creating one SSH connection
// per file or an unbounded number of helper goroutines.
func downloadFiles(paths []string) error {
	if len(paths) == 1 {
		return helper("download", paths[0])
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	jobs := make(chan string)
	var workers sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < min(maxTransfers, len(paths)); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for path := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := helperContext(ctx, "download", path); err != nil {
					failed.Add(1)
					fmt.Fprintf(os.Stderr, "sshore: %s: %s\n", cleanText(path), cleanText(err.Error()))
				}
			}
		}()
	}
queue:
	for _, path := range paths {
		select {
		case jobs <- path:
		case <-ctx.Done():
			break queue
		}
	}
	close(jobs)
	workers.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("downloads cancelled")
	}
	if n := failed.Load(); n > 0 {
		return fmt.Errorf("%d of %d downloads failed", n, len(paths))
	}
	return nil
}

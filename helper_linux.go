package main

// Approved transfers run directly on client-initiated mux streams. Each operation
// owns its files and stream; closing the stream cancels blocked I/O. Remote CLI
// helpers only request approval or detach, and never relay file bytes.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

func detachHelper() error {
	sock := os.Getenv("SSHORE_SOCKET")
	if sock == "" {
		return fmt.Errorf("run this command inside an sshore session")
	}
	c, err := net.DialTimeout("unix", sock, requestTimeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(requestTimeout))
	if err := writeMessage(c, message{Type: "detach"}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if m.Type != "detached" {
		return fileResponseError(m)
	}
	return nil
}

func sendFile(c net.Conn, f *sendingFile, id *uint64) error {
	if err := writeMessage(c, f.offer(0)); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	*id = m.ID
	if m.Type != "accept" {
		return fileResponseError(m)
	}
	// Read cancellation concurrently, interrupting writes even under backpressure.
	result := make(chan error, 1)
	go func() {
		m, err := readMessage(c)
		if err == nil && (m.Type != "result" || m.Error != "") {
			err = fileResponseError(m)
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
		return fileResponseError(m)
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
			return fileResponseError(m)
		}
	}
}

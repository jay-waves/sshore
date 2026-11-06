package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/xtaci/smux"
)

func requestOperations(ops []operation) error {
	if err := validateOperations(ops); err != nil {
		return err
	}
	sock := os.Getenv("SSHORE_SOCKET")
	if sock == "" {
		return fmt.Errorf("run this command inside an sshore session")
	}
	c, err := net.DialTimeout("unix", sock, forwardTimeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * forwardTimeout))
	if err = writeMessage(c, message{Type: "approval_request", Operations: ops}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if m.Type != "queued" {
		return fileResponseError(m)
	}
	logf(os.Stderr, logInfo, "[sshore] Request queued locally. After detach, run sshore approve from your local shell.\n")
	return nil
}
func requestFiles(kind string, paths []string) error {
	var ops []operation
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		op := operation{Kind: kind, Path: absolute}
		if kind == "download" {
			f, err := openSending(absolute)
			if err != nil {
				return err
			}
			op.Name = f.name
			op.Size = f.size
			f.close()
		} else {
			info, err := os.Stat(absolute)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("upload destination must be a directory")
			}
		}
		ops = append(ops, op)
	}
	return requestOperations(ops)
}

func (s *session) requestApproval(c net.Conn, m message) error {
	if err := validateOperations(m.Operations); err != nil {
		return err
	}
	s.mu.Lock()
	p := s.active
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("no attached client")
	}
	select {
	case p.forwardRequests <- struct{}{}:
		defer func() { <-p.forwardRequests }()
	default:
		return fmt.Errorf("too many pending requests")
	}
	st, err := p.mux.OpenStream()
	if err != nil {
		return err
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(forwardTimeout))
	if err = writeMessage(st, m); err != nil {
		return err
	}
	reply, err := readMessage(st)
	if err != nil {
		return err
	}
	if reply.Type != "queued" {
		return fileResponseError(reply)
	}
	// Acknowledge the helper before allowing the client to detach.
	_ = c.SetWriteDeadline(time.Now().Add(forwardTimeout))
	if err = writeMessage(c, message{Type: "queued"}); err != nil {
		return err
	}
	return writeMessage(st, message{Type: "queued_ack"})
}

// Only a client-initiated stream can execute an approved file operation. The
// server is not an authority for local approval; the client enforces that gate.
func approvedRemoteTransfer(st *smux.Stream, m message) {
	if len(m.Operations) != 1 || validateOperations(m.Operations) != nil {
		_ = writeMessage(st, message{Type: "error", Error: "invalid operation"})
		return
	}
	op := m.Operations[0]
	_ = st.SetDeadline(time.Time{})
	var id uint64
	var err error
	switch op.Kind {
	case "download":
		f, e := openSending(op.Path)
		if e != nil {
			err = e
			break
		}
		if f.name != op.Name || f.size != op.Size {
			f.close()
			err = fmt.Errorf("remote file metadata changed; request again")
			break
		}
		err = sendFile(st, f, &id)
		f.close()
	case "upload":
		err = helperUpload(st, op.Path, &id)
	default:
		err = fmt.Errorf("invalid transfer kind")
	}
	if err != nil {
		_ = writeMessage(st, message{Type: "error", ID: id, Error: err.Error()})
	}
}

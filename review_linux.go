package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/jay-waves/sshore/internal/files"
)

func requestOperations(_ string, ops []operation) error {
	if err := validateOperations(ops); err != nil {
		return err
	}
	sock := os.Getenv("SSHORE_SOCKET")
	if sock == "" {
		return fmt.Errorf("run this command inside an sshore shell")
	}
	c, err := dialHostedTask(context.Background(), sock, requestTimeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * requestTimeout))
	if err = writeMessage(c, message{Type: "approval_request", SoftwareVersion: version, Operations: ops}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if err = checkPeerVersion(m.SoftwareVersion, "approval host"); err != nil {
		return err
	}
	if m.Type != "approval_handoff" {
		return fileResponseError(m)
	}
	return nil
}

func (s *remoteShell) requestApproval(c net.Conn, m message) error {
	if err := checkPeerVersion(m.SoftwareVersion, "requesting helper"); err != nil {
		return err
	}
	if err := validateOperations(m.Operations); err != nil {
		return err
	}
	s.mu.Lock()
	p := s.active
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("no attached client")
	}
	return p.requestApproval(c, m, func(id string) error {
		if !validRequestID(id) {
			return fmt.Errorf("invalid approval handoff")
		}
		op := m.Operations[0]
		op.LocalHint = ""
		s.mu.Lock()
		defer s.mu.Unlock()
		s.resourceApproval = resourceApproval{}
		if op.Kind == "mount" || op.Kind == "forward" {
			s.resourceApproval = resourceApproval{id: id, op: op}
		}
		return nil
	})
}

// The authenticated file endpoint serves one operation after its approval check.
func approvedRemoteTransfer(st net.Conn, m message) {
	if len(m.Operations) != 1 || validateOperations(m.Operations) != nil {
		_ = writeMessage(st, message{Type: "error", Error: "invalid operation"})
		return
	}
	op := m.Operations[0]
	var backend *files.FS
	var err error
	switch op.Kind {
	case "download":
		var f *files.Source
		f, err = files.OpenSource(op.Path)
		if err == nil {
			defer f.Close()
			if f.Name() != op.Name || f.Size() != op.Size {
				err = fmt.Errorf("remote file metadata changed; request again")
			}
		}
		backend = files.NewSourceFS(f)
	case "upload":
		err = files.ValidateName(m.Name)
		if m.Size < 0 {
			err = fmt.Errorf("invalid upload size")
		}
		if err == nil {
			var info os.FileInfo
			info, err = os.Stat(op.Path)
			if err == nil && !info.IsDir() {
				err = fmt.Errorf("upload destination must be a directory")
			}
		}
		backend = files.NewTargetFS(filepath.Join(op.Path, m.Name), m.Size)
	default:
		err = fmt.Errorf("invalid transfer kind")
	}
	if err != nil {
		_ = writeMessage(st, message{Type: "error", Error: err.Error()})
		return
	}
	if writeMessage(st, message{Type: "sftp"}) != nil {
		return
	}
	_ = st.SetDeadline(time.Time{})
	_ = files.ServeTransfer(st, backend)
}

func (s *remoteShell) checkApprovalSource(m message) error {
	if err := checkPeerVersion(m.SoftwareVersion, "approval client"); err != nil {
		return err
	}
	if !validRequestID(m.ApprovalID) {
		return fmt.Errorf("invalid approval identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || s.pid == 0 || s.pid != m.PID || s.record.Name != m.Name || m.Instance != "" && m.Instance != s.record.UID {
		return fmt.Errorf("approved shell changed; request the operation again")
	}
	if s.active != nil {
		return fmt.Errorf("source shell remains attached; request the operation again")
	}
	return nil
}

// The helper and attachment remain connected until the local client accepts the handoff.
func (p *attachment) requestApproval(c net.Conn, m message, complete func(string) error) error {
	token := rand.Text()
	responses, err := p.approvals.open(token)
	if err != nil {
		return err
	}
	defer p.approvals.remove(token)
	timer := time.NewTimer(requestTimeout)
	defer timer.Stop()
	m.Token = token
	if err := p.controlMessage(m); err != nil {
		return err
	}
	helperDone := make(chan error, 1)
	go func() {
		var unexpected [1]byte
		_, err := io.ReadFull(c, unexpected[:])
		helperDone <- err
	}()
	cancel := func(err error) error {
		_ = p.controlMessage(message{Type: "approval_cancelled", Token: token})
		return err
	}
	var reply message
	select {
	case reply = <-responses:
	case <-p.Done():
		return io.ErrClosedPipe
	case <-helperDone:
		return cancel(io.ErrClosedPipe)
	case <-timer.C:
		return cancel(fmt.Errorf("approval request timed out"))
	}
	if reply.Type != "approval_handoff" {
		return fileResponseError(reply)
	}
	select {
	case <-p.Done():
		return io.ErrClosedPipe
	case <-helperDone:
		return cancel(io.ErrClosedPipe)
	default:
	}
	_ = c.SetWriteDeadline(time.Now().Add(requestTimeout))
	if err := writeMessage(c, message{Type: "approval_handoff", SoftwareVersion: version}); err != nil {
		return cancel(err)
	}
	// The remote invocation owns its connection. Wait until it has received
	// the handoff and closed before the frontend detaches and switches screens.
	select {
	case err := <-helperDone:
		if err != io.EOF {
			return cancel(fmt.Errorf("approval helper did not close its request"))
		}
	case <-p.Done():
		return io.ErrClosedPipe
	case <-timer.C:
		return cancel(fmt.Errorf("approval request timed out"))
	}
	if err := complete(reply.ApprovalID); err != nil {
		return cancel(err)
	}
	return p.controlMessage(message{Type: "approval_ack", Token: token})
}

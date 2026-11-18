package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/xtaci/smux"
)

func requestOperations(ops []operation) error {
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
	termui.Log(os.Stderr, termui.Info, "[sshore] Request queued. Detach, then run sshore review locally.\n")
	return nil
}
func requestFilesWithHint(kind string, paths []string, hint string) error {
	var ops []operation
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		op := operation{Kind: kind, Path: absolute, LocalHint: hint}
		if kind == "download" {
			f, err := files.OpenSource(absolute)
			if err != nil {
				return err
			}
			op.Name = f.Name()
			op.Size = f.Size()
			f.Close()
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

func (s *remoteShell) requestApproval(c net.Conn, m message) error {
	if err := validateOperations(m.Operations); err != nil {
		return err
	}
	s.mu.Lock()
	p := s.lifecycle.Active()
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("no attached client")
	}
	select {
	case p.approvalRequests <- struct{}{}:
		defer func() { <-p.approvalRequests }()
	default:
		return fmt.Errorf("too many pending requests")
	}
	st, err := p.OpenStream()
	if err != nil {
		return err
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(requestTimeout))
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
	if reply.PeerTask != nil && reply.PeerTask.State == "pending" {
		_ = storeRemoteTask(*reply.PeerTask)
	}
	// Acknowledge the helper before allowing the client to detach.
	_ = c.SetWriteDeadline(time.Now().Add(requestTimeout))
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
	var backend *files.FS
	var err error
	switch op.Kind {
	case "download":
		var f *files.Source
		f, err = files.OpenSource(op.Path)
		if err == nil {
			if f.Name() != op.Name || f.Size() != op.Size {
				err = fmt.Errorf("remote file metadata changed; request again")
			}
			f.Close()
		}
		backend = files.NewSourceFS(op.Path, op.Size)
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

// Validate the original detached shell without attaching to it. A failed
// validation must never terminate the shell or start a replacement session.
func checkApprovalSource(name string, pid int, approval string, expected ...string) error {
	if err := validSession(name); err != nil {
		return err
	}
	if pid <= 0 || !validRequestID(approval) {
		return fmt.Errorf("invalid source approval")
	}
	gate, err := setupLock()
	if err != nil {
		return err
	}
	defer gate.Close()
	socket, err := resolveSession(name, expected...)
	if err != nil {
		return err
	}
	if socket == "" {
		return fmt.Errorf("approved shell no longer exists; request the operation again")
	}
	conn, err := dialHostedTask(context.Background(), socket, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err = writeMessage(conn, message{Type: "approval_check", SoftwareVersion: version, Name: name, PID: pid, ApprovalID: approval, Instance: firstExpectedInstance(expected)}); err != nil {
		return err
	}
	m, err := readMessage(conn)
	if err != nil {
		return err
	}
	if err = checkPeerVersion(m.SoftwareVersion, "source shell"); err != nil {
		return err
	}
	if m.Type != "approval_checked" {
		return fmt.Errorf("source approval: %s", m.Error)
	}
	return nil
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
	if s.lifecycle.Exited() || s.process.PID == 0 || s.process.PID != m.PID || s.name != m.Name || m.Instance != "" && m.Instance != s.instance {
		return fmt.Errorf("approved shell changed; request the operation again")
	}
	if s.lifecycle.Active() != nil {
		return fmt.Errorf("source shell is attached; detach it before reviewing the request")
	}
	return removeRemoteTask(m.ApprovalID)
}

func runApprovedForwardWatch(args []string) error {
	if len(args) != 4 && len(args) != 5 {
		return usage("forward-watch requires client version, shell, PID and approval identity")
	}
	if err := checkPeerVersion(args[0], "approval client"); err != nil {
		return err
	}
	pid, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("invalid source shell PID")
	}
	if err = checkApprovalSource(args[1], pid, args[3], args[4:]...); err != nil {
		return err
	}
	if _, err = fmt.Fprintln(os.Stdout, forwardReadyMarker); err != nil {
		return err
	}
	return watchRemoteTask()
}

package main

import (
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"

	transportio "github.com/jay-waves/sshore/internal/transport"
)

func closedAttachment(t *testing.T) *attachment {
	t.Helper()
	a, b := net.Pipe()
	client, err := smux.Client(a, transportio.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	server, err := smux.Server(b, transportio.MuxConfig())
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	stream, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	remote, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	server.Close()
	return newAttachment(server, remote, remote)
}
func TestInitialAttachmentFailureEndsCreatedTerminal(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	s := &remoteShell{name: "failed", plain: true, done: make(chan struct{})}
	p := closedAttachment(t)
	if err := s.attach(p, message{Type: "attach", Term: "xterm-256color", Cols: 80, Rows: 24}); err == nil {
		t.Fatal("accepted closed attachment")
	}
	if s.master == nil {
		t.Fatal("test did not reach terminal startup")
	}
	defer s.master.Close()
	if !s.lifecycle.Exited() || s.lifecycle.Active() != nil {
		t.Fatal("initial failure preserved shell")
	}
	done := make(chan struct{})
	go func() { s.master.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		s.cmd.Process.Kill()
		t.Fatal("failed initial terminal survived")
	}
}
func TestFailedReattachmentPreservesDetachedTerminal(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	s := &remoteShell{name: "detached", plain: true, done: make(chan struct{})}
	if err := s.start(80, 24, "xterm-256color"); err != nil {
		t.Fatal(err)
	}
	defer func() { s.master.Close(); s.master.Wait() }()
	if err := s.attach(closedAttachment(t), message{Type: "attach", Term: "xterm-256color", Cols: 80, Rows: 24}); err == nil {
		t.Fatal("accepted closed attachment")
	}
	if s.lifecycle.Exited() || s.lifecycle.Active() != nil {
		t.Fatal("failed reconnect ended detached terminal")
	}
}

func TestDetachReportsFailedClientNotification(t *testing.T) {
	s := &remoteShell{done: make(chan struct{})}
	p := closedAttachment(t)
	if err := s.lifecycle.Attach(p, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	go s.handle(server)
	if err := writeMessage(client, message{Type: "detach"}); err != nil {
		t.Fatal(err)
	}
	reply, err := readMessage(client)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Type != "error" || reply.Error == "" {
		t.Fatalf("reported successful detach: %#v", reply)
	}
}

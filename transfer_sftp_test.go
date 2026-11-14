package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func TestTransferSFTPScope(t *testing.T) {
	dir := t.TempDir()
	backend := &transferFS{target: filepath.Join(dir, "target"), size: 2}
	a, b := net.Pipe()
	server := sftp.NewRequestServer(a, sftp.Handlers{FileGet: backend, FilePut: backend, FileCmd: backend, FileList: backend})
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve() }()
	client, err := newFTPSFTP(b, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(); client.Close(); server.Close(); <-done })
	if _, err = client.Open(filepath.ToSlash(filepath.Join(dir, "outside"))); err == nil {
		t.Fatal("arbitrary read allowed")
	}
	if _, err = client.Create("/outside"); err == nil {
		t.Fatal("arbitrary write allowed")
	}
	file, err := client.OpenFile("/.sshore-part", os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err = client.Link("/.sshore-part", "/target"); err == nil {
		t.Fatal("partial upload published")
	}
	if _, err = file.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = client.Link("/.sshore-part", "/outside"); err == nil {
		t.Fatal("arbitrary publication allowed")
	}
	if err = client.Link("/.sshore-part", "/target"); err != nil {
		t.Fatal(err)
	}
	if err = client.Link("/.sshore-part", "/target"); err == nil {
		t.Fatal("overwrite allowed")
	}
	if err = client.Remove("/target"); err == nil {
		t.Fatal("published file removed")
	}
	data, err := os.ReadFile(backend.target)
	if err != nil || string(data) != "xy" {
		t.Fatal(string(data), err)
	}
}

func TestApprovedSFTPCancelBlockedHandshake(t *testing.T) {
	client, server := muxPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go func() {
		st, err := server.AcceptStream()
		if err != nil {
			return
		}
		defer st.Close()
		_, _ = readMessage(st)
		_ = writeMessage(st, message{Type: "sftp"})
		close(ready)
		<-release
	}()
	done := make(chan error, 1)
	go func() {
		done <- executeApprovedFile(ctx, client, operation{Kind: "download", Path: "/source", Name: "source", Size: 1, LocalPath: filepath.Join(t.TempDir(), "target")}, true)
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled transfer succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not unblock SFTP")
	}
	if client.IsClosed() {
		t.Fatal("cancellation closed the attachment")
	}
}

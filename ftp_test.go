package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
)

func testFTPBackend(t *testing.T) *ftpFS {
	t.Helper()
	a, b := net.Pipe()
	server, e := sftp.NewServer(a)
	if e != nil {
		t.Fatal(e)
	}
	go func() { _ = server.Serve(); _ = server.Close() }()
	client, e := sftp.NewClientPipe(b, b)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return &ftpFS{client: client, root: filepath.ToSlash(t.TempDir())}
}
func TestFTPEditorSave(t *testing.T) {
	f := testFTPBackend(t)
	original, e := f.open("document.txt", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = original.Write([]byte("old contents")); e != nil {
		t.Fatal(e)
	}
	original.Close()
	temp, e := f.open(".save.tmp", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = temp.Write([]byte("replacement")); e != nil {
		t.Fatal(e)
	}
	if _, e = temp.WriteAt([]byte("NEW"), 0); e != nil {
		t.Fatal(e)
	}
	if e = temp.Truncate(6); e != nil {
		t.Fatal(e)
	}
	// The in-process SFTP fixture does not implement fsync; the native
	// OpenSSH test verifies durable flush separately.
	temp.Close()
	if e = f.rename(".save.tmp", "document.txt"); e != nil {
		t.Fatal(e)
	}
	result, e := f.open("document.txt", os.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer result.Close()
	data, e := io.ReadAll(result)
	if e != nil {
		t.Fatal(e)
	}
	if string(data) != "NEWlac" {
		t.Fatalf("got %q", data)
	}
	if _, e = f.stat(".save.tmp"); !os.IsNotExist(e) {
		t.Fatalf("temporary name remains: %v", e)
	}
}
func TestFTPReadOnlyAndPaths(t *testing.T) {
	f := testFTPBackend(t)
	f.readOnly = true
	if _, e := f.open("x", os.O_CREATE|os.O_RDWR, 0600); e == nil {
		t.Fatal("read-only create accepted")
	}
	if e := f.mkdir("x", 0700); e == nil {
		t.Fatal("read-only mkdir accepted")
	}
	if _, e := f.remotePath("a/../outside"); e == nil {
		t.Fatal("parent traversal accepted")
	}
	p, e := f.remotePath("/child")
	if e != nil || p != f.root+"/child" {
		t.Fatalf("root mapping: %q %v", p, e)
	}
}
func TestFTPDirectory(t *testing.T) {
	f := testFTPBackend(t)
	if e := f.mkdir("中文 folder", 0700); e != nil {
		t.Fatal(e)
	}
	dir, e := f.open("", os.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer dir.Close()
	list, e := dir.Readdir(1)
	if e != nil || len(list) != 1 || list[0].Name() != "中文 folder" {
		t.Fatalf("listing: %v %v", list, e)
	}
	if _, e = dir.Readdir(1); e != io.EOF {
		t.Fatalf("expected EOF, got %v", e)
	}
	if e = f.remove("中文 folder", true); e != nil {
		t.Fatal(e)
	}
}
func TestFTPOptions(t *testing.T) {
	o, e := parseFTPOptions([]string{"host", "/remote folder", "mount", "--read-only", "-J", "jump"})
	if e != nil || !o.readOnly || o.remote != "/remote folder" {
		t.Fatalf("%+v %v", o, e)
	}
	for _, args := range [][]string{{}, {"host", "/", "mount", "-f"}, {"--", "-bad", "/", "mount"}} {
		if _, e := parseFTPOptions(args); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestFTPDisconnectedOperations(t *testing.T) {
	f := testFTPBackend(t)
	h, err := f.open("file", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if err := f.client.Close(); err != nil {
		t.Fatal(err)
	}
	operations := []struct {
		name string
		run  func() error
	}{
		{"stat", func() error { _, err := f.stat("file"); return err }},
		{"write", func() error { _, err := h.WriteAt([]byte("x"), 0); return err }},
		{"truncate", func() error { return h.Truncate(0) }},
		{"sync", h.Sync},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- operation.run() }()
			select {
			case err := <-done:
				if err == nil || ftpErrno(err) == 0 {
					t.Fatalf("disconnected operation reported success: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("disconnected operation did not return")
			}
		})
	}
}

func TestFTPReadOnlyPreservesFiles(t *testing.T) {
	f := testFTPBackend(t)
	original := []byte("keep these bytes")
	if err := os.WriteFile(filepath.Join(f.root, "original"), original, 0600); err != nil {
		t.Fatal(err)
	}
	// Exercise handle-level guards as well as path-level mutation methods.
	h, err := f.open("original", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	f.readOnly = true
	operations := []struct {
		name string
		run  func() error
	}{
		{"write", func() error { _, err := h.WriteAt([]byte("changed"), 0); return err }},
		{"truncate", func() error { return h.Truncate(0) }},
		{"remove", func() error { return f.remove("original", false) }},
		{"rename", func() error { return f.rename("original", "renamed") }},
		{"mkdir", func() error { return f.mkdir("directory", 0700) }},
		{"create", func() error {
			file, err := f.open("created", os.O_CREATE|os.O_RDWR, 0600)
			if file != nil {
				_ = file.Close()
			}
			return err
		}},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); !errors.Is(err, syscall.EROFS) {
				t.Fatalf("expected read-only error, got %v", err)
			}
		})
	}
	data, err := os.ReadFile(filepath.Join(f.root, "original"))
	if err != nil || string(data) != string(original) {
		t.Fatalf("original changed: %q, %v", data, err)
	}
	entries, err := os.ReadDir(f.root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "original" {
		t.Fatalf("read-only operations changed directory: %v, %v", entries, err)
	}
}

func TestFTPCreationPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the local SFTP fixture cannot represent POSIX permissions on Windows")
	}
	f := testFTPBackend(t)
	h, err := f.open("private", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	checkMode := func(want os.FileMode) {
		t.Helper()
		info, err := os.Stat(filepath.Join(f.root, "private"))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("permissions = %04o, want %04o", got, want)
		}
	}
	checkMode(0600)
	if err := os.Chmod(filepath.Join(f.root, "private"), 0640); err != nil {
		t.Fatal(err)
	}
	h, err = f.open("private", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	checkMode(0640)
}

func TestFTPOpenHandleAfterRename(t *testing.T) {
	f := testFTPBackend(t)
	h, err := f.open("before", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Write([]byte("before")); err != nil {
		t.Fatal(err)
	}
	if err := f.rename("before", "after"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteAt([]byte("AFTER!"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(h)
	if err != nil || string(data) != "AFTER!" {
		t.Fatalf("renamed handle: %q, %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(f.root, "after"))
	if err != nil || string(data) != "AFTER!" {
		t.Fatalf("renamed destination: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "before")); !os.IsNotExist(err) {
		t.Fatalf("old name remains: %v", err)
	}
}

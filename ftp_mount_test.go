//go:build sshoretest

package main

// Opt-in native mount smoke: uses an in-process SFTP server and temporary files,
// never a real SSH account or persistent session. Requires existing FUSE/WinFsp.
// Run SSHORE_TEST_FTP_MOUNT=1 go test -tags=sshoretest -run TestFTPNativeMount.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestFTPNativeMount(t *testing.T) {
	if os.Getenv("SSHORE_TEST_FTP_MOUNT") != "1" {
		t.Skip("set SSHORE_TEST_FTP_MOUNT=1 to use native FUSE/WinFsp")
	}
	backend := testFTPBackend(t)
	mountpoint := filepath.Join(t.TempDir(), "mounted")
	if runtime.GOOS == "linux" {
		if e := os.Mkdir(mountpoint, 0700); e != nil {
			t.Fatal(e)
		}
	}
	mount, e := mountFTP(backend, mountpoint)
	if e != nil {
		t.Fatal(e)
	}
	unmounted := false
	defer func() {
		if !unmounted {
			if e := mount.Unmount(); e != nil {
				t.Errorf("unmount cleanup: %v", e)
			}
		}
	}()
	done := make(chan struct{})
	go func() { mount.Wait(); close(done) }()
	target := filepath.Join(mountpoint, "中文 document.txt")
	if e = os.WriteFile(target, []byte("old contents"), 0600); e != nil {
		t.Fatal(e)
	}
	temporary := filepath.Join(mountpoint, "save.tmp")
	h, e := os.OpenFile(temporary, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	if _, e = h.Write([]byte("replacement")); e != nil {
		t.Fatal(e)
	}
	if _, e = h.WriteAt([]byte("NEW"), 0); e != nil {
		t.Fatal(e)
	}
	if e = h.Truncate(6); e != nil {
		t.Fatal(e)
	}
	// The lightweight fixture has no fsync extension; OpenSSHSync separately
	// verifies fsync. Native mapping must not hide that unsupported operation.
	if e = h.Close(); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(temporary, target); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(target)
	if e != nil || string(data) != "NEWlac" {
		t.Fatalf("mounted content %q: %v", data, e)
	}
	data, e = os.ReadFile(filepath.Join(filepath.FromSlash(backend.root), "中文 document.txt"))
	if e != nil || string(data) != "NEWlac" {
		t.Fatalf("remote content %q: %v", data, e)
	}
	// A close/reopen must not retain old page-cache data after a remote edit.
	remotePath := filepath.Join(filepath.FromSlash(backend.root), "中文 document.txt")
	if e = os.WriteFile(remotePath, []byte("REMOTE"), 0600); e != nil {
		t.Fatal(e)
	}
	data, e = os.ReadFile(target)
	if e != nil || string(data) != "REMOTE" {
		t.Fatalf("reopen stale content %q: %v", data, e)
	}
	stamp := time.Unix(1700000000, 0)
	if e = os.Chtimes(target, stamp, stamp); e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(target)
	if e != nil {
		t.Fatal(e)
	}
	if info.ModTime().Unix() != stamp.Unix() {
		t.Fatalf("mtime: %s", info.ModTime())
	}
	if e = os.Remove(target); e != nil {
		t.Fatal(e)
	}
	if e = mount.Unmount(); e != nil {
		t.Fatal(e)
	}
	unmounted = true
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("mount worker did not stop")
	}
}

//go:build linux

package files

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
)

func TestLinuxSetattrMtimePreservesAtime(t *testing.T) {
	atime, mtime := uint32(1700000000), uint32(1700001000)
	backend := testFTPAgent(t)
	filename := filepath.Join(backend.root, "file")
	if err := os.WriteFile(filename, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filename, time.Unix(int64(atime), 0), time.Unix(int64(mtime), 0)); err != nil {
		t.Fatal(err)
	}
	root := &ftpNode{backend: backend}
	fs.NewNodeFS(root, nil)
	child := &ftpNode{backend: backend}
	root.AddChild("file", root.NewPersistentInode(context.Background(), child, fs.StableAttr{Mode: syscall.S_IFREG}), false)
	wanted := time.Unix(1700002000, 0)
	var in fuse.SetAttrIn
	in.Valid = fuse.FATTR_MTIME
	in.Mtime = uint64(wanted.Unix())
	var out fuse.AttrOut
	if errno := child.Setattr(context.Background(), nil, &in, &out); errno != 0 {
		t.Fatal(errno)
	}
	info, err := backend.client.Stat(backend.root + "/file")
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := info.Sys().(*sshfx.Attributes)
	if !ok {
		t.Fatalf("unexpected stat representation %T", info.Sys())
	}
	if raw.ATime != atime {
		t.Fatalf("atime changed: got %d want %d", raw.ATime, atime)
	}
	if !info.ModTime().Equal(wanted) {
		t.Fatalf("mtime=%v want %v", info.ModTime(), wanted)
	}
}

func TestLinuxSetattrModeUsesOpenHandle(t *testing.T) {
	backend := testFTPBackend(t)
	file, err := backend.open("file", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	root := &ftpNode{backend: backend}
	fs.NewNodeFS(root, nil)
	child := &ftpNode{backend: backend}
	root.AddChild("file", root.NewPersistentInode(context.Background(), child, fs.StableAttr{Mode: syscall.S_IFREG}), false)
	original := filepath.Join(backend.root, "original")
	replacement := filepath.Join(backend.root, "file")
	if err := os.Rename(replacement, original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replacement, 0644); err != nil {
		t.Fatal(err)
	}
	var in fuse.SetAttrIn
	in.Valid, in.Mode = fuse.FATTR_MODE, 0600
	var out fuse.AttrOut
	if errno := child.Setattr(context.Background(), &ftpFuseFile{file: file}, &in, &out); errno != 0 {
		t.Fatal(errno)
	}
	for name, want := range map[string]os.FileMode{original: 0600, replacement: 0644} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode %o, want %o", name, info.Mode().Perm(), want)
		}
	}
}

func TestFTPCapacitySFTP(t *testing.T) {
	f := testFTPAgent(t)
	s, err := ftpRemoteCapacity(f.client, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Blocks == 0 {
		t.Fatal("empty backing filesystem")
	}
	var out fuse.StatfsOut
	n := &ftpNode{backend: f}
	if errno := n.Statfs(context.Background(), &out); errno != 0 {
		t.Fatal(errno)
	}
	if out.Blocks != s.Blocks || uint64(out.Frsize) != s.FragmentSize || uint64(out.Bsize) != s.BlockSize {
		t.Fatalf("capacity mapping: %+v, source %+v", out, s)
	}
}

func TestMountAgentSyncAfterRename(t *testing.T) {
	backend := testFTPAgent(t)
	h, err := backend.open("saved", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	const content = "saved remotely"
	if _, err := h.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := backend.rename("saved", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := h.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(backend.root, "renamed"))
	if err != nil || string(data) != content {
		t.Fatalf("%q %v", data, err)
	}
}

func TestMountAgentSymlinkPaths(t *testing.T) {
	backend := testFTPAgent(t)
	dir := filepath.Join(backend.root, "directory")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"link": "directory", "dangling": "missing"} {
		if err := os.Symlink(target, filepath.Join(backend.root, name)); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := backend.client.RealPath(filepath.Join(backend.root, "link"))
	if err != nil || resolved != dir {
		t.Fatalf("realpath = %q, %v", resolved, err)
	}
	file := filepath.Join(backend.root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := backend.remove("file", true); err == nil {
		t.Fatal("rmdir accepted a regular file")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	if err := backend.remove("link", true); err == nil {
		t.Fatal("rmdir accepted a symlink")
	}
	for _, name := range []string{"link", "dangling"} {
		if err := backend.remove(name, false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(backend.root, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", name, err)
		}
	}
	if err := backend.remove("directory", true); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxWarmReaddirUsesNoSFTPRequests(t *testing.T) {
	backend, wire := testFTPBackendWithWire(t)
	root := &ftpNode{backend: backend}
	fs.NewNodeFS(root, nil)
	for range 2 {
		listing, errno := root.Readdir(context.Background())
		if errno != 0 {
			t.Fatal(errno)
		}
		listing.Close()
	}
	wire.mu.Lock()
	defer wire.mu.Unlock()
	if stat, open := wire.totals[17].Started, wire.totals[11].Started; stat != 0 || open != 1 {
		t.Fatalf("two listings sent %d STAT and %d OPENDIR requests", stat, open)
	}
}

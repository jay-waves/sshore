package files

import (
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func testFTPBackend(t *testing.T) *ftpFS {
	t.Helper()
	backend, _ := testFTPBackendWithWire(t)
	return backend
}
func testFTPBackendWithWire(t *testing.T) (*ftpFS, *ftpWire) {
	t.Helper()
	a, b := net.Pipe()
	server, e := sftp.NewServer(a)
	if e != nil {
		t.Fatal(e)
	}
	go func() { _ = server.Serve(); _ = server.Close() }()
	wire := newFTPWire(b, b)
	wire.metrics = true
	client, e := sftp.NewClientPipe(wire, wire)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return &ftpFS{client: client, root: filepath.ToSlash(t.TempDir())}, wire
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

func TestFTPDirectoryCacheAcrossHandles(t *testing.T) {
	backend, wire := testFTPBackendWithWire(t)
	if err := backend.mkdir("a", 0700); err != nil {
		t.Fatal(err)
	}
	if err := backend.mkdir("b", 0700); err != nil {
		t.Fatal(err)
	}
	first, err := backend.open("", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := backend.open("", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	a, err := first.Readdir(1)
	if err != nil {
		t.Fatal(err)
	}
	wire.mu.Lock()
	before := wire.totals[12].Started
	wire.mu.Unlock()
	all, err := second.Readdir(-1)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := first.Readdir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || len(rest) != 1 || len(all) != 2 || a[0].Name() != all[0].Name() || rest[0].Name() != all[1].Name() {
		t.Fatal("directory cursors interfered")
	}
	wire.mu.Lock()
	after := wire.totals[12].Started
	wire.mu.Unlock()
	if before == 0 || after != before {
		t.Fatalf("repeat listing added READDIR requests: %d -> %d", before, after)
	}
	if backend.cache.counters.Hits != 1 {
		t.Fatalf("reopen did not reuse listing: %+v", backend.cache.counters)
	}
	if err := backend.mkdir("c", 0700); err != nil {
		t.Fatal(err)
	}
	third, err := backend.open("", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	updated, err := third.Readdir(-1)
	wire.mu.Lock()
	refreshed := wire.totals[12].Started
	wire.mu.Unlock()
	if refreshed <= after {
		t.Fatal("mutation did not force a remote directory refresh")
	}
	if err != nil || len(updated) != 3 {
		t.Fatalf("stale directory after mkdir: %v %v", updated, err)
	}
}

func TestFTPDirectoryCacheDoesNotRenewAttributes(t *testing.T) {
	backend, wire := testFTPBackendWithWire(t)
	if err := backend.mkdir("child", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.readDirectory(backend.root); err != nil {
		t.Fatal(err)
	}
	key := "s:" + backend.root + "/child"
	backend.cache.mu.Lock()
	e := backend.cache.entries[key]
	if e == nil {
		backend.cache.mu.Unlock()
		t.Fatal("fresh listing did not seed metadata")
	}
	v := e.Value.(ftpCacheValue)
	v.expires = time.Now().Add(-time.Second)
	e.Value = v
	backend.cache.mu.Unlock()
	if _, err := backend.readDirectory(backend.root); err != nil {
		t.Fatal(err)
	}
	wire.mu.Lock()
	before := wire.totals[17].Started
	wire.mu.Unlock()
	if _, err := backend.stat("child"); err != nil {
		t.Fatal(err)
	}
	wire.mu.Lock()
	after := wire.totals[17].Started
	wire.mu.Unlock()
	if after != before+1 {
		t.Fatal("cached listing renewed expired attributes")
	}
}

type ftpCapacityStub struct {
	version string
	stat    *sftp.StatVFS
	err     error
	path    string
}

func (s *ftpCapacityStub) HasExtension(string) (string, bool)      { return s.version, s.version != "" }
func (s *ftpCapacityStub) StatVFS(p string) (*sftp.StatVFS, error) { s.path = p; return s.stat, s.err }

func TestFTPCapacityNegotiation(t *testing.T) {
	for _, version := range []string{"", "1", "3"} {
		c := &ftpCapacityStub{version: version}
		if _, err := ftpRemoteCapacity(c, "/data"); !errors.Is(err, syscall.ENOTSUP) || c.path != "" {
			t.Fatalf("version %q: err=%v path=%q", version, err, c.path)
		}
	}
	c := &ftpCapacityStub{version: "2", stat: &sftp.StatVFS{Bsize: 4096, Frsize: 512, Blocks: 100, Bfree: 40, Bavail: 30}}
	s, err := ftpRemoteCapacity(c, "/data")
	if err != nil || s != c.stat || c.path != "/data" {
		t.Fatalf("stat=%v err=%v path=%q", s, err, c.path)
	}
	c.err = syscall.EACCES
	if _, err := ftpRemoteCapacity(c, "/data"); !errors.Is(err, syscall.EACCES) {
		t.Fatal(err)
	}
}

func TestFTPCapacityInvalidReplies(t *testing.T) {
	for _, s := range []*sftp.StatVFS{
		nil,
		{},
		{Bsize: 4096, Frsize: 4096, Blocks: math.MaxUint64},
		{Bsize: 4096, Frsize: 4096, Blocks: 10, Bfree: 11},
		{Bsize: 4096, Frsize: 4096, Blocks: 10, Bfree: 5, Bavail: 6},
		{Bsize: math.MaxUint64, Frsize: 4096},
		{Bsize: 4096, Frsize: 4096, Namemax: math.MaxUint64},
	} {
		if err := validateFTPCapacity(s); !errors.Is(err, syscall.EIO) {
			t.Fatalf("%+v: %v", s, err)
		}
	}
}

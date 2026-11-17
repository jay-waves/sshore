package files

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestFTPRequestStats(t *testing.T) {
	wire := newFTPWire(nil, nil)
	wire.metrics = true
	for _, id := range []uint32{1, 2} {
		packet := make([]byte, 5)
		packet[0] = 17
		binary.BigEndian.PutUint32(packet[1:], id)
		if err := wire.record(packet); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uint32{2, 1} {
		packet := make([]byte, 9)
		packet[0] = 101
		binary.BigEndian.PutUint32(packet[1:], id)
		if id == 2 {
			binary.BigEndian.PutUint32(packet[5:], 3)
		}
		if _, err := wire.filter(packet); err != nil {
			t.Fatal(err)
		}
	}
	totals := wire.totals[17]
	if totals.Started != 2 || totals.Completed != 2 || totals.Errors != 1 || totals.SentBytes != 18 || totals.ReceivedBytes != 26 || totals.TotalNS < totals.MaxNS || wire.peakRequests != 2 {
		t.Fatalf("%+v peak=%d", totals, wire.peakRequests)
	}
	backend := &ftpFS{metrics: &ftpCallbackMetrics{}}
	backend.metrics.observe("Open")()
	var output bytes.Buffer
	writeFTPStats(wire, backend, &output)
	var report struct {
		Type      string
		Requests  map[string]ftpRequestTotals
		Callbacks map[string]ftpCallbackTotals
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Type != "mount_stats" || report.Requests["STAT"].Completed != 2 || report.Callbacks["Open"].Calls != 1 {
		t.Fatalf("%s", output.Bytes())
	}
}
func TestFTPStatsFinalReport(t *testing.T) {
	var output bytes.Buffer
	stop := startFTPStats(newFTPWire(nil, nil), &ftpFS{}, &output)
	stop()
	if output.Len() == 0 {
		t.Fatal("missing final statistics")
	}
}

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
	if runtime.GOOS == "windows" {
		t.Skip("in-process SFTP fixture needs Linux remote filesystem rename semantics")
	}
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

func TestFTPDirectoryBudgetKeepsConnection(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if e := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); e != nil {
			t.Fatal(e)
		}
	}
	for _, limit := range []string{"entries", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			a, b := net.Pipe()
			server, e := sftp.NewServer(a)
			if e != nil {
				t.Fatal(e)
			}
			go func() { _ = server.Serve(); _ = server.Close() }()
			wire := newFTPWire(b, b)
			if limit == "entries" {
				wire.maxEntries = 2
			} else {
				wire.maxDirectoryBytes = 16
			}
			client, e := sftp.NewClientPipe(wire, wire)
			if e != nil {
				t.Fatal(e)
			}
			defer server.Close()
			defer client.Close()
			_, e = client.ReadDir(filepath.ToSlash(root))
			if e == nil || !strings.Contains(e.Error(), "listing limit") {
				t.Fatalf("expected budget failure, got %v", e)
			}
			if _, e = client.Stat(filepath.ToSlash(filepath.Join(root, "a"))); e != nil {
				t.Fatalf("unrelated operation failed: %v", e)
			}
			wire.mu.Lock()
			defer wire.mu.Unlock()
			if len(wire.directories) != 0 || len(wire.requests) != 0 {
				t.Fatalf("leaked directory/request state: %d/%d", len(wire.directories), len(wire.requests))
			}
		})
	}
}
func TestFTPRejectsOversizedPacket(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], ftpMaxPacket+1)
	wire := newFTPWire(bytes.NewReader(header[:]), nil)
	if _, e := wire.Read(make([]byte, 1)); e == nil {
		t.Fatal("accepted oversized response")
	}
}

type ftpTestMount struct{ unmount func() error }

func (m ftpTestMount) Unmount() error { return m.unmount() }
func (m ftpTestMount) Wait()          {}
func TestFTPGracefulUnmount(t *testing.T) {
	for _, failure := range []error{nil, syscall.EBUSY} {
		aborted := false
		stopped, e := stopFTPMount(ftpTestMount{func() error { return failure }}, nil, func() { aborted = true }, time.Second)
		if aborted || stopped != (failure == nil) || !errors.Is(e, failure) {
			t.Fatalf("stopped=%v aborted=%v error=%v", stopped, aborted, e)
		}
	}
}
func TestFTPForcedUnmount(t *testing.T) {
	for _, kind := range []string{"timeout", "signal"} {
		t.Run(kind, func(t *testing.T) {
			entered, released := make(chan struct{}), make(chan struct{})
			signals := make(chan os.Signal, 1)
			grace := 10 * time.Millisecond
			if kind == "signal" {
				grace = time.Second
				go func() { <-entered; signals <- os.Interrupt }()
			}
			stopped, e := stopFTPMount(ftpTestMount{func() error { close(entered); <-released; return nil }}, signals, func() { close(released) }, grace)
			if !stopped || e == nil {
				t.Fatalf("forced unmount lost error: %v %v", stopped, e)
			}
		})
	}
}
func TestFTPWireFragmentedWrites(t *testing.T) {
	var output bytes.Buffer
	wire := newFTPWire(nil, ftpWriteCloser{&output})
	packet := []byte{0, 0, 0, 5, 1, 0, 0, 0, 3}
	for _, b := range packet {
		if _, e := wire.Write([]byte{b}); e != nil {
			t.Fatal(e)
		}
	}
	if !bytes.Equal(output.Bytes(), packet) {
		t.Fatalf("wire changed frame: %v", output.Bytes())
	}
}

type ftpWriteCloser struct{ io.Writer }

func (ftpWriteCloser) Close() error { return nil }

func TestFTPCapacityCache(t *testing.T) {
	f, wire := testFTPBackendWithWire(t)
	_, firstErr := f.cachedCapacity()
	count := func() uint64 { wire.mu.Lock(); defer wire.mu.Unlock(); return wire.totals[200].Started }
	before := count()
	for range 20 {
		if _, err := f.cachedCapacity(); (err == nil) != (firstErr == nil) {
			t.Fatalf("cached capacity error changed: %v", err)
		}
	}
	if count() != before {
		t.Fatal("repeated capacity query reached server")
	}
	for _, invalidate := range []func(){
		func() { f.cache.beginMutation(f.root + "/file")() },
		func() { f.cache.invalidateDirectory(f.root) },
		func() {
			f.cache.mu.Lock()
			e := f.cache.entries["v:"+f.root]
			v := e.Value.(ftpCacheValue)
			v.expires = time.Now().Add(-time.Second)
			e.Value = v
			f.cache.mu.Unlock()
		},
	} {
		invalidate()
		_, _ = f.cachedCapacity()
		if count() != before+1 {
			t.Fatal("capacity was not refreshed")
		}
		before = count()
	}
}

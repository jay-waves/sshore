package files

import (
	"context"
	"github.com/pkg/sftp"
	"net"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"
)

type delayedDirectoryConn struct{ net.Conn }

func (c delayedDirectoryConn) Write(b []byte) (int, error) {
	time.Sleep(10 * time.Millisecond)
	return c.Conn.Write(b)
}
func TestDirectoryPrefetchWithResponseLatency(t *testing.T) {
	a, b := net.Pipe()
	server, err := sftp.NewServer(delayedDirectoryConn{a})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(); server.Close() }()
	wire := newFTPWire(b, b)
	wire.metrics = true
	client, err := sftp.NewClientPipe(wire, wire)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { b.Close(); client.Close(); server.Close() }()
	f := &ftpFS{client: client, root: filepath.ToSlash(t.TempDir())}
	target := path.Join(f.root, "child")
	if err = os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err = f.readDirectory(target); err != nil {
		t.Fatal(err)
	}
	cold := time.Since(start)
	f.cache.invalidateRemote()
	f.prefetch = startDirectoryPrefetch(context.Background(), f)
	defer func() { f.prefetch.cancel(); b.Close(); <-f.prefetch.done }()
	if _, err = f.readDirectory(f.root); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		f.cache.mu.Lock()
		e := f.cache.entries["d:"+target]
		f.cache.mu.Unlock()
		if e != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prefetch not ready")
		}
		time.Sleep(time.Millisecond)
	}
	f.prefetch.cancel()
	<-f.prefetch.done
	wire.mu.Lock()
	before := wire.totals[12].Started
	wire.mu.Unlock()
	start = time.Now()
	if _, err = f.readDirectory(target); err != nil {
		t.Fatal(err)
	}
	warm := time.Since(start)
	wire.mu.Lock()
	extra := wire.totals[12].Started - before
	wire.mu.Unlock()
	if extra != 0 {
		t.Fatalf("warm directory sent %d READDIR requests", extra)
	}
	t.Logf("10ms per response write: cold=%s prefetched=%s extra_READDIR=%d", cold, warm, extra)
}

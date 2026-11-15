package mount

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHandleStatPollingAndInvalidation(t *testing.T) {
	backend, wire := testFTPBackendWithWire(t)
	f, err := backend.open("file", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	count := func() uint64 { wire.mu.Lock(); defer wire.mu.Unlock(); return wire.totals[8].Started } // FSTAT
	check := func(size int64) {
		t.Helper()
		info, err := f.cachedStat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != size {
			t.Fatalf("size %d, want %d", info.Size(), size)
		}
	}
	before := count()
	for i := 0; i < 100; i++ {
		check(0)
	}
	if got := count() - before; got != 1 {
		t.Fatalf("100 handle polls sent %d FSTAT requests", got)
	}
	if _, err = f.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	check(3)
	if err = f.Truncate(1); err != nil {
		t.Fatal(err)
	}
	check(1)
	if err = os.WriteFile(filepath.Join(backend.root, "file"), []byte("remote"), 0600); err != nil {
		t.Fatal(err)
	}
	backend.cache.invalidateRemote()
	check(6)
	// Expire deterministically; hits must not extend the original TTL.
	backend.cache.mu.Lock()
	e := backend.cache.entries[f.statCacheKey()]
	v := e.Value.(ftpCacheValue)
	v.expires = time.Now().Add(-time.Second)
	e.Value = v
	backend.cache.mu.Unlock()
	before = count()
	check(6)
	if count() != before+1 {
		t.Fatal("expired handle metadata was reused")
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.cachedStat(); err != os.ErrClosed {
		t.Fatalf("closed handle: %v", err)
	}
	backend.cache.mu.Lock()
	defer backend.cache.mu.Unlock()
	if backend.cache.entries[f.statCacheKey()] != nil {
		t.Fatal("closed handle retained metadata")
	}
}

package files

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsDirectoryReopenSharesMetadata(t *testing.T) {
	backend, wire := testFTPBackendWithWire(t)
	if err := os.WriteFile(filepath.Join(backend.root, "entry"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	// GetSecurityByName, Open, then gofs's separate enumeration handle.
	if _, err := backend.stat(""); err != nil {
		t.Fatal(err)
	}
	var firstReadDirs uint64
	for i := 0; i < 10; i++ {
		h, err := backend.openWithMetadata("", os.O_RDONLY, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = h.cachedStat(); err != nil {
			h.Close()
			t.Fatal(err)
		}
		entries, err := h.Readdir(-1)
		h.Close()
		if err != nil || len(entries) != 1 {
			t.Fatalf("entries=%v err=%v", entries, err)
		}
		wire.mu.Lock()
		stats, dirs := wire.totals[17].Started, wire.totals[12].Started
		wire.mu.Unlock()
		if i == 0 {
			firstReadDirs = dirs
		}
		if stats != 1 || dirs != firstReadDirs {
			t.Fatalf("reopen %d: STAT=%d READDIR=%d; want 1/%d", i, stats, dirs, firstReadDirs)
		}
	}
	if err := os.WriteFile(filepath.Join(backend.root, "new"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	backend.cache.invalidateRemote()
	h, err := backend.openWithMetadata("", os.O_RDONLY, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err = h.cachedStat(); err != nil {
		t.Fatal(err)
	}
	entries, err := h.Readdir(-1)
	if err != nil || len(entries) != 2 {
		t.Fatalf("stale listing: %v %v", entries, err)
	}
	wire.mu.Lock()
	stats := wire.totals[17].Started
	wire.mu.Unlock()
	if stats != 2 {
		t.Fatalf("invalidation STAT=%d want 2", stats)
	}
}

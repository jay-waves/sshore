package mount

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsOpenReusesPathQuery(t *testing.T) {
	backend, wire := testFTPBackendWithWire(t)
	target := filepath.Join(backend.root, "file")
	if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	// Equivalent to WinFsp GetSecurityByName followed by Open and GetFileInfo.
	if _, err := backend.stat("file"); err != nil {
		t.Fatal(err)
	}
	// Replace the path between the attribute check and open: first handle stat
	// and reads must describe the newly opened file, not the cached pathname.
	if err := os.Rename(target, target+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := backend.openWithMetadata("file", os.O_RDONLY, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.cachedStat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 11 {
		t.Fatalf("used stale path size: %d", info.Size())
	}
	wire.mu.Lock()
	stats, opens, fstats := wire.totals[17].Started, wire.totals[3].Started, wire.totals[8].Started
	wire.mu.Unlock()
	if stats != 1 || opens != 1 || fstats != 1 {
		t.Fatalf("STAT=%d OPEN=%d FSTAT=%d; want 1 each", stats, opens, fstats)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// Mutating opens must not reuse a stale regular-file classification.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if f, err := backend.openWithMetadata("file", os.O_WRONLY|os.O_TRUNC, 0, true); err == nil {
		f.Close()
		t.Fatal("truncated a directory")
	}
}

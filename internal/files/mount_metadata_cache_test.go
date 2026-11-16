package files

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

type ftpCacheInfo struct {
	os.FileInfo
	name string
	mode os.FileMode
}

func (i ftpCacheInfo) Name() string      { return i.name }
func (i ftpCacheInfo) Mode() os.FileMode { return i.mode }

func TestFTPMetadataReuseAndExpiry(t *testing.T) {
	var cache ftpMetadataCache
	calls := 0
	fetch := func() (os.FileInfo, error) {
		calls++
		return ftpCacheInfo{name: "file"}, nil
	}
	for range 20 {
		if _, err := cache.stat("/file", fetch); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("20 repeated queries made %d remote requests", calls)
	}
	for e := cache.lru.Front(); e != nil; e = e.Next() {
		v := e.Value.(ftpCacheValue)
		v.expires = time.Now().Add(-time.Second)
		e.Value = v
	}
	_, _ = cache.stat("/file", fetch)
	if calls != 2 {
		t.Fatal("expired metadata was reused")
	}
	for range 2 {
		_, err := cache.stat("/missing", func() (os.FileInfo, error) {
			calls++
			return nil, os.ErrNotExist
		})
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if calls != 4 {
		t.Fatal("errors were cached")
	}
}

func TestFTPMetadataDirectoryBudgetAndLinks(t *testing.T) {
	var cache ftpMetadataCache
	epoch := cache.snapshot()
	cache.directory(epoch, "/dir", []os.FileInfo{
		ftpCacheInfo{name: "file"}, ftpCacheInfo{name: "link", mode: os.ModeSymlink},
		ftpCacheInfo{name: ".."}, ftpCacheInfo{name: "a/b"},
	})
	if len(cache.entries) != 1 {
		t.Fatalf("unexpected cached entries: %v", cache.entries)
	}
	_, err := cache.stat("/dir/file", func() (os.FileInfo, error) {
		t.Fatal("directory metadata did not avoid a remote Stat")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]os.FileInfo, ftpMetadataLimit+1)
	for i := range entries {
		entries[i] = ftpCacheInfo{name: fmt.Sprint(i)}
	}
	cache.directory(epoch, "/dir", entries)
	if len(cache.entries) != ftpMetadataLimit {
		t.Fatalf("cache budget: %d", len(cache.entries))
	}
	cache.beginMutation()()
	epoch.started = time.Now().Add(-2 * ftpMetadataTTL)
	cache.directory(epoch, "/dir", entries[:1])
	if len(cache.entries) != 0 {
		t.Fatal("an old directory snapshot refreshed expired entries")
	}
}

func TestFTPMetadataMutationRejectsInflightReads(t *testing.T) {
	var cache ftpMetadataCache
	info := ftpCacheInfo{name: "file"}
	old := cache.snapshot()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.stat("/file", func() (os.FileInfo, error) {
			close(started)
			<-release
			return info, nil
		})
	}()
	<-started
	completeMutation := cache.beginMutation()
	completeMutation()
	close(release)
	<-done
	if len(cache.entries) != 0 {
		t.Fatal("in-flight response repopulated invalidated metadata")
	}
	cache.directory(old, "/", []os.FileInfo{info})
	if len(cache.entries) != 0 {
		t.Fatal("in-flight directory read repopulated invalidated metadata")
	}
	finish1, finish2 := cache.beginMutation(), cache.beginMutation()
	finish1()
	_, _ = cache.stat("/file", func() (os.FileInfo, error) { return info, nil })
	if len(cache.entries) != 0 {
		t.Fatal("cached a read while a second mutation was still active")
	}
	finish2()
	_, _ = cache.stat("/file", func() (os.FileInfo, error) { return info, nil })
	if len(cache.entries) != 1 {
		t.Fatal("cache did not recover after mutations")
	}
	finish := cache.beginMutation()
	if len(cache.entries) != 0 {
		t.Fatal("mutation did not invalidate existing entries")
	}
	finish()
}

func TestFTPCacheLRU(t *testing.T) {
	var cache ftpMetadataCache
	fetch := func() (os.FileInfo, error) { return ftpCacheInfo{name: "file"}, nil }
	for i := 0; i < ftpMetadataLimit; i++ {
		_, _ = cache.stat(fmt.Sprint(i), fetch)
	}
	_, _ = cache.stat("0", fetch)
	_, _ = cache.stat("new", fetch)
	if cache.entries["s:0"] == nil || cache.entries["s:1"] != nil || cache.entries["s:new"] == nil {
		t.Fatal("LRU did not preserve hot entry and admit new entry")
	}
	if cache.counters.Evictions != 1 {
		t.Fatal(cache.counters)
	}
}

func TestFTPCacheConcurrentReads(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint(directory), func(t *testing.T) {
			var cache ftpMetadataCache
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 2)
			fetch := func() (ftpCacheValue, error) {
				close(entered)
				<-release
				return ftpCacheValue{info: ftpCacheInfo{name: "file"}, directory: directory, bytes: 256}, nil
			}
			go func() { _, err := cache.load("key", time.Second, fetch); done <- err }()
			<-entered
			go func() { _, err := cache.load("key", time.Second, fetch); done <- err }()
			// Observe the waiter rather than relying on scheduler timing.
			deadline := time.After(2 * time.Second)
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			waiting := false
			for !waiting {
				cache.mu.Lock()
				waiting = cache.counters.Shared == 1
				cache.mu.Unlock()
				if waiting {
					break
				}
				select {
				case <-tick.C:
				case <-deadline:
					close(release)
					t.Fatal("second read did not join in-flight query")
				}
			}
			close(release)
			for range 2 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestFTPCacheDirectoryReuseAndBudget(t *testing.T) {
	var cache ftpMetadataCache
	calls := 0
	fetch := func() ([]os.FileInfo, error) {
		calls++
		return []os.FileInfo{ftpCacheInfo{name: "a"}, ftpCacheInfo{name: "b"}}, nil
	}
	a, err := cache.readDir("/", fetch)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cache.readDir("/", fetch)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(a) != 2 || len(b) != 2 {
		t.Fatalf("calls=%d a=%v b=%v", calls, a, b)
	}
	cache.beginMutation()()
	_, _ = cache.readDir("/", fetch)
	if calls != 2 {
		t.Fatal("directory survived mutation")
	}
	huge := make([]os.FileInfo, ftpCacheDirectoryEntries+1)
	for i := range huge {
		huge[i] = ftpCacheInfo{name: "x"}
	}
	list, err := cache.readDir("/huge", func() ([]os.FileInfo, error) { return huge, nil })
	if err != nil || len(list) != len(huge) {
		t.Fatal("uncacheable directory was truncated")
	}
	if cache.entries["d:/huge"] != nil {
		t.Fatal("oversized directory retained")
	}
	for i := 0; i < ftpDirectoryCacheLimit+1; i++ {
		_, _ = cache.readDir(fmt.Sprint(i), fetch)
	}
	if cache.dirs > ftpDirectoryCacheLimit || cache.bytes > ftpCacheBytes || cache.dirEntries > ftpCacheDirectoryEntries {
		t.Fatal("cache budget exceeded")
	}
}

func TestFTPCacheMutationSeparatesFlights(t *testing.T) {
	var cache ftpMetadataCache
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.stat("file", func() (os.FileInfo, error) { close(entered); <-release; return ftpCacheInfo{name: "old"}, nil })
	}()
	<-entered
	cache.beginMutation()()
	info, err := cache.stat("file", func() (os.FileInfo, error) { return ftpCacheInfo{name: "new"}, nil })
	close(release)
	<-done
	if err != nil || info.Name() != "new" {
		t.Fatal("new read joined obsolete query")
	}
	info, err = cache.stat("file", func() (os.FileInfo, error) { t.Fatal("new result not cached"); return nil, nil })
	if err != nil || info.Name() != "new" {
		t.Fatal("old completion replaced new cache entry")
	}
}

type ftpCacheExtendedInfo struct {
	ftpCacheInfo
	extended []sftp.StatExtended
}

func (i ftpCacheExtendedInfo) Extended() []sftp.StatExtended { return i.extended }
func TestFTPCacheExtendedAttributeBudget(t *testing.T) {
	var cache ftpMetadataCache
	// Reuse the backing string to exercise accounting without a large allocation.
	payload := strings.Repeat("x", 16*1024)
	ext := make([]sftp.StatExtended, 1024)
	for i := range ext {
		ext[i] = sftp.StatExtended{ExtType: "test", ExtData: payload}
	}
	info := ftpCacheExtendedInfo{ftpCacheInfo: ftpCacheInfo{name: "file"}, extended: ext}
	fetch := func() (os.FileInfo, error) { return info, nil }
	_, _ = cache.stat("a", fetch)
	_, _ = cache.stat("b", fetch)
	if cache.entries["s:a"] != nil || cache.entries["s:b"] == nil || cache.bytes > ftpCacheBytes {
		t.Fatal("extended attributes escaped byte budget")
	}
	info.extended = append(ext, ext...)
	_, err := cache.stat("oversized", fetch)
	if err != nil || cache.entries["s:oversized"] != nil {
		t.Fatal("oversized attributes must be returned without retention")
	}
}

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

func TestFTPCacheScopedMutation(t *testing.T) {
	var cache ftpMetadataCache
	fetch := func() (os.FileInfo, error) { return ftpCacheInfo{name: "file"}, nil }
	for _, name := range []string{"/a/file", "/a/sibling", "/b/file"} {
		_, _ = cache.stat(name, fetch)
	}
	_, _ = cache.readDir("/a", func() ([]os.FileInfo, error) { return nil, nil })
	finish1 := cache.beginMutation("/a/file")
	finish2 := cache.beginMutation("/a/file")
	if cache.entries["s:/a/file"] != nil || cache.entries["d:/a"] != nil {
		t.Fatal("mutation retained affected metadata")
	}
	for _, name := range []string{"/a/sibling", "/b/file"} {
		_, _ = cache.stat(name, func() (os.FileInfo, error) {
			t.Fatalf("mutation discarded unrelated cache: %s", name)
			return nil, nil
		})
	}
	finish1()
	_, _ = cache.stat("/a/file", fetch)
	if cache.entries["s:/a/file"] != nil {
		t.Fatal("cached metadata while overlapping mutation was active")
	}
	_, _ = cache.stat("/b/new", fetch)
	if cache.entries["s:/b/new"] == nil {
		t.Fatal("unrelated cache population blocked by mutation")
	}
	finish2()
	_, _ = cache.stat("/a/file", fetch)
	if cache.entries["s:/a/file"] == nil || len(cache.mutations) != 0 {
		t.Fatal("cache did not recover after mutation")
	}
}

func TestFTPCacheScopedMutationRejectsInflightDirectory(t *testing.T) {
	var cache ftpMetadataCache
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.readDir("/a", func() ([]os.FileInfo, error) {
			close(entered)
			<-release
			return []os.FileInfo{ftpCacheInfo{name: "file"}}, nil
		})
	}()
	<-entered
	cache.beginMutation("/a/file")()
	close(release)
	<-done
	if len(cache.entries) != 0 {
		t.Fatal("old directory response repopulated changed file metadata")
	}
}

func TestFTPCacheRenameInvalidatesBothTreesAndHandles(t *testing.T) {
	var cache ftpMetadataCache
	keys := map[string]string{
		"s:/a/tree/child": "", "d:/a/tree/sub": "", "l:/b/tree/link": "",
		"d:/a": "", "d:/b": "", "h:source": "/a/tree/sub", "h:destination": "/b",
	}
	for key, scope := range keys {
		_, _ = cache.loadScoped(key, scope, time.Second, func() (ftpCacheValue, error) {
			return ftpCacheValue{info: ftpCacheInfo{name: "file"}}, nil
		})
	}
	_, _ = cache.stat("/unrelated/file", func() (os.FileInfo, error) { return ftpCacheInfo{name: "file"}, nil })
	cache.beginMutation("/a/tree", "/b/tree")()
	if len(cache.entries) != 1 || cache.entries["s:/unrelated/file"] == nil {
		t.Fatalf("unexpected entries after rename: %v", cache.entries)
	}
}

func TestFTPWritePreservesUnrelatedMetadata(t *testing.T) {
	backend, wire := testFTPBackendWithWire(t)
	for _, name := range []string{"changed", "untouched"} {
		if err := os.WriteFile(filepath.Join(backend.root, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.stat(name); err != nil {
			t.Fatal(err)
		}
	}
	f, err := backend.open("changed", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("longer"), 0); err != nil {
		t.Fatal(err)
	}
	wire.mu.Lock()
	before := wire.totals[17].Started
	wire.mu.Unlock()
	if _, err := backend.stat("untouched"); err != nil {
		t.Fatal(err)
	}
	wire.mu.Lock()
	after := wire.totals[17].Started
	wire.mu.Unlock()
	if after != before {
		t.Fatal("write caused an unrelated remote STAT")
	}
	info, err := backend.stat("changed")
	if err != nil || info.Size() != 6 {
		t.Fatalf("changed file metadata not refreshed: %v %v", info, err)
	}
}

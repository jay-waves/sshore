package files

import (
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"testing"
	"time"
)

func TestDirectoryPrefetchBoundedAndUseful(t *testing.T) {
	fs, wire := testFTPBackendWithWire(t)
	for _, name := range append([]string{".git"}, prefetchTestNames(directoryPrefetchLimit+2)...) {
		if err := os.MkdirAll(path.Join(fs.root, name, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	fs.prefetch = startDirectoryPrefetch(context.Background(), fs)
	defer func() { fs.prefetch.cancel(); <-fs.prefetch.done }()
	if _, err := fs.readDirectory(fs.root); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		fs.cache.mu.Lock()
		ready := true
		for _, name := range prefetchTestNames(directoryPrefetchLimit) {
			if fs.cache.entries["d:"+path.Join(fs.root, name)] == nil {
				ready = false
			}
		}
		fs.cache.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			fs.cache.mu.Lock()
			var keys []string
			for key := range fs.cache.entries {
				keys = append(keys, key)
			}
			fs.cache.mu.Unlock()
			t.Fatalf("child directories were not prefetched: %v", keys)
		}
		time.Sleep(time.Millisecond)
	}
	fs.prefetch.cancel()
	<-fs.prefetch.done
	fs.cache.mu.Lock()
	for _, name := range []string{".git", "dir16", "dir17", "dir00/nested"} {
		if fs.cache.entries["d:"+path.Join(fs.root, name)] != nil {
			t.Error("unexpected speculative read", name)
		}
	}
	fs.cache.mu.Unlock()
	count := func() uint64 { wire.mu.Lock(); defer wire.mu.Unlock(); return wire.totals[12].Started }
	before := count()
	entries, err := fs.readDirectory(path.Join(fs.root, "dir00"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("prefetched listing: %v %v", entries, err)
	}
	if count() != before {
		t.Fatal("entering prefetched child sent another READDIR")
	}
	if err = os.Mkdir(path.Join(fs.root, "dir00", "changed"), 0700); err != nil {
		t.Fatal(err)
	}
	fs.cache.invalidateRemote()
	entries, err = fs.readDirectory(path.Join(fs.root, "dir00"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("stale prefetched listing: %v %v", entries, err)
	}
}

func TestDirectoryPrefetchQueueReplaced(t *testing.T) {
	p := &directoryPrefetch{ctx: context.Background(), wake: make(chan struct{}, 1)}
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		if err := os.Mkdir(path.Join(dir, fmt.Sprint(i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var infos []os.FileInfo
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, info)
	}
	slices.Reverse(infos)
	p.schedule("/old", infos)
	p.schedule("/new", infos)
	if len(p.paths) != directoryPrefetchLimit {
		t.Fatalf("queue length %d", len(p.paths))
	}
	for i, name := range p.paths {
		if name != path.Join("/new", entries[i].Name()) {
			t.Fatalf("prefetch selection %d: %s", i, name)
		}
		if path.Dir(name) != "/new" {
			t.Fatal("stale queued path", name)
		}
	}
}

func prefetchTestNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("dir%02d", i)
	}
	return names
}

func TestPrefetchReplacementCancelsActiveBatch(t *testing.T) {
	p := &directoryPrefetch{ctx: context.Background(), wake: make(chan struct{}, directoryPrefetchWorkers)}
	p.schedule("/old", nil)
	old := p.batch
	p.schedule("/old", nil)
	if p.batch != old || old.Err() != nil {
		t.Fatal("duplicate browse restarted active prefetch")
	}
	p.schedule("/new", nil)
	if old.Err() != context.Canceled {
		t.Fatal("old batch was not cancelled")
	}
	if p.batch.Err() != nil {
		t.Fatal("replacement already cancelled")
	}
	p.cancelBatch()
}

func TestWatchRegistrationPreservesSiblingPrefetch(t *testing.T) {
	var cache ftpMetadataCache
	fetch := func() ([]os.FileInfo, error) { return []os.FileInfo{ftpCacheInfo{name: "old"}}, nil }
	if _, err := cache.readDir("/sibling", fetch); err != nil {
		t.Fatal(err)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.readDir("/new", func() ([]os.FileInfo, error) { close(started); <-release; return fetch() })
	}()
	<-started
	cache.registered("/new")
	close(release)
	<-done
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries["d:/sibling"] == nil {
		t.Fatal("unrelated prefetch was evicted")
	}
	if cache.entries["s:/new/old"] != nil {
		t.Fatal("pre-registration attributes repopulated cache")
	}
	if cache.entries["d:/new"] != nil {
		t.Fatal("pre-registration response repopulated cache")
	}
}

func TestRegistrationScopesOpenHandleCache(t *testing.T) {
	var cache ftpMetadataCache
	fetch := func() (ftpCacheValue, error) { return ftpCacheValue{info: ftpCacheInfo{name: "file"}, bytes: 256}, nil }
	for _, dir := range []string{"/one", "/two"} {
		if _, err := cache.loadScoped("h:"+dir, dir, time.Second, fetch); err != nil {
			t.Fatal(err)
		}
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.loadScoped("h:pending", "/one", time.Second, func() (ftpCacheValue, error) { close(started); <-release; return fetch() })
	}()
	<-started
	cache.registered("/one")
	close(release)
	<-done
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries["h:/one"] != nil || cache.entries["h:pending"] != nil {
		t.Fatal("affected handle metadata survived confirmation")
	}
	if cache.entries["h:/two"] == nil {
		t.Fatal("unrelated open handle metadata was invalidated")
	}
	if cache.counters.RemoteInvalidations != 0 || cache.counters.SubscriptionConfirmations != 1 {
		t.Fatal("confirmation counted as remote file change")
	}
}

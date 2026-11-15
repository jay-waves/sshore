package files

import (
	"fmt"
	"os"
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
	epoch.started = time.Now().Add(-2 * time.Second)
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

package files

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/jay-waves/sshore/tests/testutil"
	sftp "github.com/pkg/sftp/v2"
)

func TestDirectoryPrefetchBoundedAndUseful(t *testing.T) {
	fs, wire := testFTPBackendWithWire(t)
	for _, name := range []string{".git", "dir00", "dir01", "dir02", "dir03", "dir04", "dir05", "dir06", "dir07", "dir16", "dir17"} {
		if err := os.MkdirAll(path.Join(fs.root, name, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	fs.prefetch = startDirectoryPrefetch(ctx, fs, nil)
	defer func() { cancel(); <-fs.prefetch.done }()
	if _, err := fs.readDirectory(fs.root); err != nil {
		t.Fatal(err)
	}
	if got := fs.prefetch.latest.targets; got[0] != path.Join(fs.root, "dir00") || got[9] != path.Join(fs.root, "dir17") {
		t.Fatalf("ranked siblings = %v", got)
	}
	deadline := time.Now().Add(time.Second)
	for {
		fs.cache.mu.Lock()
		ready := true
		for _, name := range []string{"dir00", "dir01", "dir02", "dir03", "dir04", "dir05", "dir06", "dir07", "dir16", "dir17"} {
			if fs.cache.entries[ftpCacheKey{kind: ftpDirectoryKey, name: path.Join(fs.root, name)}] == nil {
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
				keys = append(keys, key.name)
			}
			fs.cache.mu.Unlock()
			t.Fatalf("child directories were not prefetched: %v", keys)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-fs.prefetch.done
	fs.cache.mu.Lock()
	for _, name := range []string{".git", "dir00/nested"} {
		if fs.cache.entries[ftpCacheKey{kind: ftpDirectoryKey, name: path.Join(fs.root, name)}] != nil {
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

func TestDirectoryPrefetchLearnsForegroundVisits(t *testing.T) {
	fs, _ := testFTPBackendWithWire(t)
	for _, name := range []string{"src", "docs", "a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		if err := os.Mkdir(path.Join(fs.root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &directoryPrefetch{fs: fs, ctx: ctx, delta: VisitHistory{}, pending: make(chan *directoryPrefetchBatch, 1)}
	fs.prefetch = p
	visit := func(dir string) {
		t.Helper()
		if _, err := fs.readDirectory(path.Join(fs.root, dir)); err != nil {
			t.Fatal(err)
		}
	}
	visit("")
	if p.latest != nil {
		t.Fatal("cold wide directory scheduled prefetch")
	}
	visit("src")
	visit("src")
	visit("")
	if p.latest == nil || p.latest.targets[0] != path.Join(fs.root, "src") || p.latest.targets[1] != "" {
		t.Fatal("expected only visited child")
	}
	visit("src")
	visit("")
	if p.latest == nil || p.latest.targets[0] != path.Join(fs.root, "src") {
		t.Fatal("missing repeat prediction")
	}
	if len(p.delta) != 1 || p.delta["src"].Score > 2 {
		t.Fatal("unexpected visit history", p.delta)
	}
	batch := p.latest.ctx
	visit("docs")
	if batch.Err() != context.Canceled {
		t.Fatal("navigation did not cancel previous batch")
	}
	if p.latest != nil {
		t.Fatal("stale prediction")
	}
}
func TestDirectoryHistoryDecayAndBound(t *testing.T) {
	now := time.Now()
	h := VisitHistory{}
	h.add("old", 4, now.Add(-14*24*time.Hour))
	h.add("recent", 2, now)
	if h["old"].weight(now) >= h["recent"].weight(now) {
		t.Fatal("history did not fade")
	}
	h.Merge(nil, now.Add(31*24*time.Hour))
	if len(h) != 0 {
		t.Fatal("expired history retained")
	}
	for i := 0; i < VisitHistoryLimit+1; i++ {
		h.add(fmt.Sprint(i), 1, now)
	}
	if len(h) != VisitHistoryLimit {
		t.Fatal("unbounded history", len(h))
	}
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
	if cache.entries[ftpCacheKey{kind: ftpDirectoryKey, name: "/sibling"}] == nil {
		t.Fatal("unrelated prefetch was evicted")
	}
	if cache.entries[ftpCacheKey{kind: ftpStatKey, name: "/new/old"}] != nil {
		t.Fatal("pre-registration attributes repopulated cache")
	}
	if cache.entries[ftpCacheKey{kind: ftpDirectoryKey, name: "/new"}] != nil {
		t.Fatal("pre-registration response repopulated cache")
	}
}

func TestRegistrationScopesOpenHandleCache(t *testing.T) {
	var cache ftpMetadataCache
	fetch := func() (ftpCacheValue, error) { return ftpCacheValue{info: ftpCacheInfo{name: "file"}, bytes: 256}, nil }
	one, two, pending := testHandleKey("/one"), testHandleKey("/two"), testHandleKey("/one")
	for _, key := range []ftpCacheKey{one, two} {
		if _, err := cache.load(key, fetch); err != nil {
			t.Fatal(err)
		}
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.load(pending, func() (ftpCacheValue, error) { close(started); <-release; return fetch() })
	}()
	<-started
	cache.registered("/one")
	close(release)
	<-done
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries[one] != nil || cache.entries[pending] != nil {
		t.Fatal("affected handle metadata survived confirmation")
	}
	if cache.entries[two] == nil {
		t.Fatal("unrelated open handle metadata was invalidated")
	}
	if cache.counters.RemoteInvalidations != 0 || cache.counters.SubscriptionConfirmations != 1 {
		t.Fatal("confirmation counted as remote file change")
	}
}

type delayedDirectoryConn struct{ net.Conn }

func (c delayedDirectoryConn) Write(b []byte) (int, error) {
	time.Sleep(10 * time.Millisecond)
	return c.Conn.Write(b)
}
func TestDirectoryPrefetchWithResponseLatency(t *testing.T) {
	a, b := testutil.TCPPair(t)
	go serveTestSFTP(delayedDirectoryConn{a})
	wire := newFTPWire(b, b)
	wire.enableMetrics()
	client, err := sftp.NewClientPipe(t.Context(), wire, wire)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { b.Close(); client.Close() }()
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
	ctx, cancel := context.WithCancel(context.Background())
	f.prefetch = startDirectoryPrefetch(ctx, f, VisitHistory{"child": {2, time.Now()}})
	defer func() { cancel(); b.Close(); <-f.prefetch.done }()
	if _, err = f.readDirectory(f.root); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		f.cache.mu.Lock()
		e := f.cache.entries[ftpCacheKey{kind: ftpDirectoryKey, name: target}]
		f.cache.mu.Unlock()
		if e != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prefetch not ready")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
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

func TestMetadataRefreshPrioritizesRecentAccessAndBoundsQueue(t *testing.T) {
	fs := &ftpFS{}
	r := &metadataRefresh{fs: fs, ctx: context.Background(), wake: make(chan struct{}, 1)}
	fs.cache.refresh = r
	var order []int
	old, recent := testHandleKey("/unrelated"), testHandleKey("/unrelated")
	r.fetch = func(_ context.Context, key ftpCacheKey) (ftpCacheValue, error) {
		if key == old {
			order = append(order, 0)
		} else {
			order = append(order, 1)
		}
		return ftpCacheValue{info: ftpCacheInfo{name: "file"}, bytes: 256}, nil
	}
	for _, key := range []ftpCacheKey{old, recent} {
		_, _ = fs.cache.load(key, func() (ftpCacheValue, error) { return r.fetch(context.Background(), key) })
	}
	order = nil
	fs.cache.beginMutation("/changed/file")()
	_, _ = fs.cache.load(old, func() (ftpCacheValue, error) {
		t.Fatal("foreground hit waited for verification")
		return ftpCacheValue{}, nil
	})
	makeRefreshReady(r)
	r.step()
	if len(order) != 1 || order[0] != 0 {
		t.Fatalf("latest access was not prioritized: %v", order)
	}
	for i := 0; i < metadataRefreshLimit+20; i++ {
		name := string(rune('a' + i))
		_, _ = fs.cache.stat(name, func() (os.FileInfo, error) { return ftpCacheInfo{name: name}, nil })
	}
	fs.cache.mu.Lock()
	fs.cache.revision++
	for i := 0; i < metadataRefreshLimit+20; i++ {
		key := ftpCacheKey{kind: ftpStatKey, name: string(rune('a' + i))}
		r.enqueueLocked(fs.cache.entries[key])
	}
	fs.cache.mu.Unlock()
	if len(r.queue) != metadataRefreshLimit {
		t.Fatalf("queue length: %d", len(r.queue))
	}
}

func TestMetadataRefreshDoesNotBlockForegroundOrPublishAfterMutation(t *testing.T) {
	fs := &ftpFS{}
	r := &metadataRefresh{fs: fs, ctx: context.Background(), wake: make(chan struct{}, 1)}
	fs.cache.refresh = r
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	calls := 0
	fetch := func() (ftpCacheValue, error) {
		calls++
		if calls == 2 {
			close(entered)
			<-release
		}
		return ftpCacheValue{info: ftpCacheInfo{name: "old"}, bytes: 256}, nil
	}
	key := testHandleKey("/alias")
	r.fetch = func(context.Context, ftpCacheKey) (ftpCacheValue, error) { return fetch() }
	_, _ = fs.cache.load(key, fetch)
	fs.cache.beginMutation("/changed/file")()
	makeRefreshReady(r)
	go func() { r.step(); close(done) }()
	<-entered
	foreground := make(chan error, 1)
	go func() {
		_, err := fs.cache.load(key, func() (ftpCacheValue, error) {
			return ftpCacheValue{}, errors.New("foreground unexpectedly fetched")
		})
		foreground <- err
	}()
	select {
	case err := <-foreground:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("foreground waited for background verification")
	}
	fs.cache.beginMutation("/changed/file")()
	close(release)
	<-done
	if fs.cache.counters.RefreshDiscarded != 1 || fs.cache.entries[key].Value.(ftpCacheValue).revision == fs.cache.revision {
		t.Fatal("verification from before a later mutation was published")
	}
}

func TestMetadataRefreshUpdatesHardLinkAliasWithoutForegroundRead(t *testing.T) {
	fs, _ := testFTPBackendWithWire(t)
	if err := os.WriteFile(filepath.Join(fs.root, "original"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(fs.root, "original"), filepath.Join(fs.root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.stat("alias"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := startMetadataRefresh(ctx, fs)
	defer func() { cancel(); <-r.done }()
	f, err := fs.open("original", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("longer"), 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		fs.cache.mu.Lock()
		e := fs.cache.entries[ftpCacheKey{kind: ftpStatKey, name: filepath.ToSlash(filepath.Join(fs.root, "alias"))}]
		updated := e != nil && e.Value.(ftpCacheValue).info.Size() == 6 && fs.cache.counters.RefreshCompleted != 0
		fs.cache.mu.Unlock()
		if updated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("alias was not actively verified in the background")
		}
		time.Sleep(time.Millisecond)
	}
}

// Advance only queue readiness; tests need not sleep through debounce windows.
func makeRefreshReady(r *metadataRefresh) {
	r.fs.cache.mu.Lock()
	defer r.fs.cache.mu.Unlock()
	ready := time.Now().Add(-time.Second)
	for i := range r.queue {
		r.queue[i].first, r.queue[i].last = ready, ready
	}
}

func TestMetadataRefreshDebounceDeadlineAndRateLimit(t *testing.T) {
	fs := &ftpFS{}
	r := &metadataRefresh{fs: fs, ctx: context.Background(), wake: make(chan struct{}, 1)}
	key := testHandleKey("/alias")
	_, _ = fs.cache.load(key, func() (ftpCacheValue, error) {
		return ftpCacheValue{info: ftpCacheInfo{name: "file"}, bytes: 256}, nil
	})
	fs.cache.revision++
	now := time.Now()
	entry := fs.cache.entries[key]
	r.queue = append(r.queue, metadataRefreshTask{entry: entry, first: now, last: now})
	if _, delay := r.nextLocked(now); delay != metadataRefreshDebounce {
		t.Fatalf("quiet debounce = %s", delay)
	}
	// Repeated activity postpones quiet time but preserves the original deadline.
	r.queue[0].last = now.Add(450 * time.Millisecond)
	if _, delay := r.nextLocked(now.Add(450 * time.Millisecond)); delay != 50*time.Millisecond {
		t.Fatalf("continuous activity exceeded max debounce: %s", delay)
	}
	if index, delay := r.nextLocked(now.Add(metadataRefreshMaxWait)); index < 0 || r.queue[index].entry != entry || delay != 0 {
		t.Fatalf("deadline did not release work: index=%d delay=%s", index, delay)
	}
	r.nextAllowed = now.Add(metadataRefreshMaxWait + metadataRefreshInterval)
	if _, delay := r.nextLocked(now.Add(metadataRefreshMaxWait)); delay != metadataRefreshInterval {
		t.Fatalf("rate limit bypassed: %s", delay)
	}
	if _, delay := r.nextLocked(now.Add(2 * time.Minute)); delay != -1 || len(r.queue) != 0 {
		t.Fatal("expired work consumed request capacity")
	}
}

func TestMetadataRefreshFailureBackoffAndCancellation(t *testing.T) {
	fs := &ftpFS{}
	ctx, cancel := context.WithCancel(context.Background())
	r := &metadataRefresh{fs: fs, ctx: ctx, wake: make(chan struct{}, 1), done: make(chan struct{})}
	fs.cache.refresh = r
	calls := 0
	failed := testHandleKey("/alias")
	fetch := func() (ftpCacheValue, error) {
		calls++
		if calls > 1 {
			return ftpCacheValue{}, errors.New("transport unavailable")
		}
		return ftpCacheValue{info: ftpCacheInfo{name: "file"}, bytes: 256}, nil
	}
	r.fetch = func(context.Context, ftpCacheKey) (ftpCacheValue, error) { return fetch() }
	_, _ = fs.cache.load(failed, fetch)
	fs.cache.beginMutation("/changed/file")()
	makeRefreshReady(r)
	before := time.Now()
	r.step()
	if r.nextAllowed.Before(before.Add(500*time.Millisecond)) || r.failures != 1 || fs.cache.entries[failed] != nil {
		t.Fatal("failure did not back off and retire stale metadata")
	}
	_, err := fs.cache.load(testHandleKey("/other"), func() (ftpCacheValue, error) {
		return ftpCacheValue{info: ftpCacheInfo{name: "foreground"}, bytes: 256}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fs.cache.beginMutation("/changed/file")()
	go r.run()
	cancel()
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for the background timer")
	}
}

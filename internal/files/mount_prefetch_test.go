package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pkg/sftp"
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

type prefetchListingGate struct {
	entered chan string
	release chan struct{}
}

func (g *prefetchListingGate) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	<-g.release
	return emptyPrefetchListing{}, nil
}

type emptyPrefetchListing struct{}

func (emptyPrefetchListing) ListAt([]os.FileInfo, int64) (int, error) { return 0, io.EOF }

func TestDirectoryPrefetchOverlapsNetworkWaits(t *testing.T) {
	a, b := net.Pipe()
	gate := &prefetchListingGate{entered: make(chan string, directoryPrefetchWorkers+1), release: make(chan struct{})}
	server := sftp.NewRequestServer(a, sftp.Handlers{FileList: gate})
	go func() { _ = server.Serve() }()
	client, err := sftp.NewClientPipe(b, &prefetchRequestRecorder{WriteCloser: b, started: gate.entered})
	if err != nil {
		t.Fatal(err)
	}
	fs := &ftpFS{client: client, root: "/"}
	p := startDirectoryPrefetch(context.Background(), fs)
	defer func() {
		p.cancel()
		close(gate.release)
		b.Close()
		client.Close()
		server.Close()
		<-p.done
	}()
	p.mu.Lock()
	p.batch, p.cancelBatch = context.WithCancel(p.ctx)
	for i := 0; i <= directoryPrefetchWorkers; i++ {
		p.paths = append(p.paths, fmt.Sprintf("/child%d", i))
	}
	p.mu.Unlock()
	for i := 0; i < directoryPrefetchWorkers; i++ {
		p.wake <- struct{}{}
	}
	for i := 0; i < directoryPrefetchWorkers; i++ {
		select {
		case <-gate.entered:
		case <-time.After(time.Second):
			t.Fatal("prefetch serialized independent network waits")
		}
	}
	select {
	case path := <-gate.entered:
		t.Fatal("concurrency exceeded limit", path)
	default:
	}
}

type prefetchRequestRecorder struct {
	io.WriteCloser
	started chan string
}

func (w *prefetchRequestRecorder) Write(b []byte) (int, error) {
	if len(b) >= 5 && b[4] == 11 {
		w.started <- "OPENDIR"
	}
	return w.WriteCloser.Write(b)
}

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

func TestMetadataRefreshPrioritizesRecentAccessAndBoundsQueue(t *testing.T) {
	fs := &ftpFS{}
	r := &metadataRefresh{fs: fs, ctx: context.Background(), wake: make(chan struct{}, 1), queue: make(map[string]metadataRefreshTask)}
	fs.cache.refresh = r
	var order []string
	for _, key := range []string{"h:old", "h:recent"} {
		key := key
		_, _ = fs.cache.loadScoped(key, "/unrelated", time.Minute, func() (ftpCacheValue, error) {
			order = append(order, key)
			return ftpCacheValue{info: ftpCacheInfo{name: key}, bytes: 256}, nil
		})
	}
	order = nil
	fs.cache.beginMutation("/changed/file")()
	_, _ = fs.cache.loadScoped("h:old", "/unrelated", time.Minute, func() (ftpCacheValue, error) {
		t.Fatal("foreground hit waited for verification")
		return ftpCacheValue{}, nil
	})
	makeRefreshReady(r)
	r.step()
	if len(order) != 1 || order[0] != "h:old" {
		t.Fatalf("latest access was not prioritized: %v", order)
	}
	for i := 0; i < metadataRefreshLimit+20; i++ {
		key := "s:" + string(rune('a'+i))
		_, _ = fs.cache.stat(key[2:], func() (os.FileInfo, error) { return ftpCacheInfo{name: key}, nil })
		fs.cache.mu.Lock()
		r.scheduleLocked(key)
		fs.cache.mu.Unlock()
	}
	if len(r.queue) != metadataRefreshLimit {
		t.Fatalf("queue length: %d", len(r.queue))
	}
}

func TestMetadataRefreshDoesNotBlockForegroundOrPublishAfterMutation(t *testing.T) {
	fs := &ftpFS{}
	r := &metadataRefresh{fs: fs, ctx: context.Background(), wake: make(chan struct{}, 1), queue: make(map[string]metadataRefreshTask)}
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
	_, _ = fs.cache.loadScoped("h:file", "/alias", time.Minute, fetch)
	fs.cache.beginMutation("/changed/file")()
	makeRefreshReady(r)
	go func() { r.step(); close(done) }()
	<-entered
	foreground := make(chan error, 1)
	go func() {
		_, err := fs.cache.loadScoped("h:file", "/alias", time.Minute, func() (ftpCacheValue, error) {
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
	if fs.cache.counters.RefreshDiscarded != 1 || fs.cache.entries["h:file"].Value.(ftpCacheValue).revision == fs.cache.revision {
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
	r := startMetadataRefresh(context.Background(), fs)
	defer func() { r.cancel(); <-r.done }()
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
		e := fs.cache.entries["s:"+filepath.ToSlash(filepath.Join(fs.root, "alias"))]
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
	for key, task := range r.queue {
		task.first, task.last = time.Now().Add(-time.Second), time.Now().Add(-time.Second)
		r.queue[key] = task
	}
}

func TestMetadataRefreshDebounceDeadlineAndRateLimit(t *testing.T) {
	fs := &ftpFS{}
	r := &metadataRefresh{fs: fs, ctx: context.Background(), wake: make(chan struct{}, 1), queue: make(map[string]metadataRefreshTask)}
	_, _ = fs.cache.loadScoped("h:file", "/alias", time.Minute, func() (ftpCacheValue, error) {
		return ftpCacheValue{info: ftpCacheInfo{name: "file"}, bytes: 256}, nil
	})
	fs.cache.revision++
	now := time.Now()
	r.queue["h:file"] = metadataRefreshTask{priority: 1, first: now, last: now}
	if _, delay := r.nextLocked(now); delay != metadataRefreshDebounce {
		t.Fatalf("quiet debounce = %s", delay)
	}
	// Repeated activity postpones quiet time but preserves the original deadline.
	task := r.queue["h:file"]
	task.last = now.Add(450 * time.Millisecond)
	r.queue["h:file"] = task
	if _, delay := r.nextLocked(now.Add(450 * time.Millisecond)); delay != 50*time.Millisecond {
		t.Fatalf("continuous activity exceeded max debounce: %s", delay)
	}
	if key, delay := r.nextLocked(now.Add(metadataRefreshMaxWait)); key != "h:file" || delay != 0 {
		t.Fatalf("deadline did not release work: %q %s", key, delay)
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
	r := &metadataRefresh{fs: fs, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}), queue: make(map[string]metadataRefreshTask)}
	fs.cache.refresh = r
	calls := 0
	_, _ = fs.cache.loadScoped("h:failed", "/alias", time.Minute, func() (ftpCacheValue, error) {
		calls++
		if calls > 1 {
			return ftpCacheValue{}, errors.New("transport unavailable")
		}
		return ftpCacheValue{info: ftpCacheInfo{name: "file"}, bytes: 256}, nil
	})
	fs.cache.beginMutation("/changed/file")()
	makeRefreshReady(r)
	before := time.Now()
	r.step()
	if r.nextAllowed.Before(before.Add(500*time.Millisecond)) || r.failures != 1 || fs.cache.entries["h:failed"] != nil {
		t.Fatal("failure did not back off and retire stale metadata")
	}
	_, err := fs.cache.loadScoped("h:foreground", "/other", time.Minute, func() (ftpCacheValue, error) {
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

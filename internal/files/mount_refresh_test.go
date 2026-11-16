package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

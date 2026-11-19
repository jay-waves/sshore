package files

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func TestFTPNotifyInvalidationDuringFetch(t *testing.T) {
	var cache ftpMetadataCache
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.stat("/file", func() (os.FileInfo, error) { close(entered); <-release; return ftpCacheInfo{name: "old"}, nil })
	}()
	<-entered
	cache.invalidateRemote()
	close(release)
	<-done
	if len(cache.entries) != 0 {
		t.Fatal("stale in-flight result repopulated cache")
	}
	if cache.counters.RemoteInvalidations != 1 {
		t.Fatal(cache.counters)
	}
}

func TestFTPNotifyInvalidationBeforeSubscriptionAck(t *testing.T) {
	a, b := net.Pipe()
	server, err := smux.Server(a, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := smux.Client(b, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	local, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	var cache ftpMetadataCache
	_, _ = cache.stat("/file", func() (os.FileInfo, error) { return ftpCacheInfo{name: "old"}, nil })
	n := startFTPNotifications(local, &cache)
	defer n.Close()
	done := make(chan struct{})
	go func() { n.ensure(context.Background(), "/"); close(done) }()
	m, err := readMessage(remote)
	if err != nil || m.Type != "watch" {
		t.Fatalf("%+v %v", m, err)
	}
	if err = writeMessage(remote, message{Type: "watch_reset"}); err != nil {
		t.Fatal(err)
	}
	if err = writeMessage(remote, message{Type: "watch_ack"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("invalidation deadlocked subscription")
	}
	n.mu.Lock()
	generation := n.generation
	n.mu.Unlock()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != 0 || generation != 1 {
		t.Fatal("reset did not invalidate cached metadata and subscriptions")
	}
}

func TestNewDirectoriesDoNotWaitForWatchAck(t *testing.T) {
	backend := testFTPBackend(t)
	for _, dir := range []string{"first", "second"} {
		if err := os.Mkdir(filepath.Join(backend.root, dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(backend.root, dir, "file"), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a, b := net.Pipe()
	server, err := smux.Server(a, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := smux.Client(b, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	local, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	n := startFTPNotifications(local, &backend.cache)
	defer n.Close()
	backend.notifications = n
	first := filepath.Join(backend.root, "first")
	n.enqueue(first)
	if _, err = readMessage(remote); err != nil {
		t.Fatal(err)
	}
	// Deliberately withhold the ACK. Listing multiple previously unseen paths
	// must finish independently, without waiting for notification timeout.
	done := make(chan error, 1)
	go func() {
		for _, dir := range []string{"first", "second"} {
			entries, e := backend.readDirectory(filepath.Join(backend.root, dir))
			if e != nil {
				done <- e
				return
			}
			if len(entries) != 1 {
				done <- fmt.Errorf("directory %s: %d entries", dir, len(entries))
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("new directory blocked on watch acknowledgement")
	}
	// Mutate before the watch was established: ACK must retire cached listings.
	if err = os.WriteFile(filepath.Join(first, "new"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = writeMessage(remote, message{Type: "watch_ack"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		backend.cache.mu.Lock()
		invalidations := backend.cache.counters.SubscriptionConfirmations
		backend.cache.mu.Unlock()
		if invalidations > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ACK did not invalidate pre-subscription cache")
		}
		time.Sleep(time.Millisecond)
	}
	entries, err := backend.readDirectory(first)
	if err != nil || len(entries) != 2 {
		t.Fatalf("stale pre-subscription listing: %v %v", entries, err)
	}
	n.mu.Lock()
	pending := 0
	for _, entry := range n.dirs {
		if entry.ready != nil {
			pending++
		}
	}
	n.mu.Unlock()
	if pending > ftpWatchLimit+1 {
		t.Fatalf("unbounded subscriptions: %d", pending)
	}
}

func TestWatchRenewalPreservesContinuousCache(t *testing.T) {
	a, b := net.Pipe()
	server, err := smux.Server(a, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := smux.Client(b, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	local, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	var cache ftpMetadataCache
	n := startFTPNotifications(local, &cache)
	defer n.Close()
	exchange := func(failure string, reset bool) {
		t.Helper()
		n.mu.Lock()
		if v, ok := n.dirs["/dir"]; ok {
			v.expires = time.Now().Add(-time.Second)
			n.dirs["/dir"] = v
		}
		n.mu.Unlock()
		done := make(chan error, 1)
		go func() {
			_ = remote.SetDeadline(time.Now().Add(time.Second))
			_, e := readMessage(remote)
			if e == nil && reset {
				e = writeMessage(remote, message{Type: "watch_reset"})
			}
			if e == nil {
				e = writeMessage(remote, message{Type: "watch_ack", Error: failure})
			}
			done <- e
		}()
		n.ensure(context.Background(), "/dir")
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	exchange("", false)
	seed := func() {
		_, err := cache.stat("/dir/file", func() (os.FileInfo, error) { return ftpCacheInfo{name: "file"}, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	has := func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.entries[ftpCacheKey{kind: ftpStatKey, name: "/dir/file"}] != nil
	}
	seed()
	cache.mu.Lock()
	expires := cache.entries[ftpCacheKey{kind: ftpStatKey, name: "/dir/file"}].Value.(ftpCacheValue).expires
	cache.mu.Unlock()
	exchange("", false)
	if !has() {
		t.Fatal("continuous renewal discarded valid cache")
	}
	cache.mu.Lock()
	renewed := cache.entries[ftpCacheKey{kind: ftpStatKey, name: "/dir/file"}].Value.(ftpCacheValue).expires
	cache.mu.Unlock()
	if renewed != expires {
		t.Fatal("renewal extended cache TTL")
	}
	exchange("", true)
	if has() {
		t.Fatal("reset retained stale cache")
	}
	seed()
	exchange("unavailable", false)
	if has() {
		t.Fatal("failed renewal retained cache")
	}
	seed()
	exchange("", false)
	if has() {
		t.Fatal("recovery retained pre-subscription data")
	}
}

func TestWatchChangesBounded(t *testing.T) {
	var changes watchChanges
	for i := 0; i < 100; i++ {
		changes.add("/same", false)
	}
	if _, ok := changes.dirs["/same"]; changes.reset || len(changes.dirs) != 1 || !ok {
		t.Fatal(changes)
	}
	changes = watchChanges{}
	for i := 0; i <= ftpWatchLimit; i++ {
		changes.add(fmt.Sprintf("/dir%d", i), false)
	}
	if !changes.reset || len(changes.dirs) != 0 {
		t.Fatal("overflow lost reset", changes)
	}
	changes = watchChanges{}
	changes.add("/a", false)
	changes.add("", true)
	changes.add("/b", false)
	if !changes.reset || len(changes.dirs) != 0 {
		t.Fatal("reset did not dominate")
	}
}
func TestPrefetchStatistics(t *testing.T) {
	var c ftpMetadataCache
	fetch := func() ([]os.FileInfo, error) { return []os.FileInfo{ftpCacheInfo{name: "file"}}, nil }
	_, _ = c.readDirMode(context.Background(), "/used", true, func(context.Context) ([]os.FileInfo, error) { return fetch() })
	_, _ = c.readDirMode(context.Background(), "/unused", true, func(context.Context) ([]os.FileInfo, error) { return fetch() })
	_, _ = c.readDir("/used", fetch)
	_, _ = c.readDir("/used", fetch)
	c.invalidateRemote()
	if c.counters.PrefetchStored != 2 || c.counters.PrefetchHits != 1 || c.counters.PrefetchDiscarded != 1 {
		t.Fatal(c.counters)
	}
}

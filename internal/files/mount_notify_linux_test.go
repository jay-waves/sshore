//go:build linux

package files

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func testFTPAgent(t *testing.T) *ftpFS {
	t.Helper()
	if _, err := ftpSFTPServerExecutable(); err != nil {
		t.Skip(err)
	}
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- ServeAgent(a, "test-release") }()
	t.Cleanup(func() {
		b.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("mount agent did not exit")
		}
	})
	mux, data, events, err := OpenMux(b, "test-release")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mux.Close() })
	client, err := sftp.NewClientPipe(data, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	f := &ftpFS{client: client, root: t.TempDir()}
	f.notifications = startFTPNotifications(events, &f.cache)
	t.Cleanup(f.notifications.Close)
	return f
}

func waitFTPInvalidation(t *testing.T, f *ftpFS, before uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		f.cache.mu.Lock()
		n := f.cache.counters.RemoteInvalidations
		f.cache.mu.Unlock()
		if n > before {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("remote mutation did not invalidate cache")
}
func ftpInvalidations(f *ftpFS) uint64 {
	f.cache.mu.Lock()
	defer f.cache.mu.Unlock()
	return f.cache.counters.RemoteInvalidations
}

func TestFTPNotifyRemoteChanges(t *testing.T) {
	f := testFTPAgent(t)
	f.notifications.ensure(f.root)
	filename := filepath.Join(f.root, "file")
	if err := os.WriteFile(filename, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.readDirectory(f.root); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func() error
		size   int64
	}{
		{"write", func() error { return os.WriteFile(filename, []byte("longer"), 0600) }, 6},
		{"atomic-replace", func() error {
			if err := os.WriteFile(filename+".tmp", []byte("replacement"), 0600); err != nil {
				return err
			}
			return os.Rename(filename+".tmp", filename)
		}, 11},
		{"remove", func() error { return os.Remove(filename) }, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.stat("file"); err != nil {
				t.Fatal(err)
			}
			before := ftpInvalidations(f)
			if err := tc.mutate(); err != nil {
				t.Fatal(err)
			}
			waitFTPInvalidation(t, f, before)
			info, err := f.stat("file")
			if tc.size < 0 {
				if !os.IsNotExist(err) {
					t.Fatalf("deleted file: %v", err)
				}
			} else if err != nil || info.Size() != tc.size {
				t.Fatalf("stale metadata: %v %v", info, err)
			}
		})
	}
	// A browsed subdirectory gets its own watch; no recursive tree scan is needed.
	sub := filepath.Join(f.root, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.readDirectory(sub); err != nil {
		t.Fatal(err)
	}
	f.notifications.ensure(sub)
	before := ftpInvalidations(f)
	if err := os.WriteFile(filepath.Join(sub, "new"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	waitFTPInvalidation(t, f, before)
	entries, err := f.readDirectory(sub)
	if err != nil || len(entries) != 1 {
		t.Fatalf("stale directory: %v %v", entries, err)
	}
	// Proxied OpenSSH extensions retain their writable-mount behavior.
	h, err := f.open("synced", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err = h.Write([]byte("saved")); err != nil {
		t.Fatal(err)
	}
	if err = h.Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestFTPNotifyDisconnectKeepsSFTP(t *testing.T) {
	f := testFTPAgent(t)
	if _, err := f.readDirectory(f.root); err != nil {
		t.Fatal(err)
	}
	f.notifications.stream.Close()
	select {
	case <-f.notifications.done:
	case <-time.After(time.Second):
		t.Fatal("notification reader stuck")
	}
	if err := os.WriteFile(filepath.Join(f.root, "after"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := f.readDirectory(f.root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("notification failure broke SFTP: %v %v", entries, err)
	}
	h, err := f.open("after", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	content, err := io.ReadAll(h)
	if err != nil || string(content) != "data" {
		t.Fatalf("%q %v", content, err)
	}
}

func TestFTPNotifyWatchBudget(t *testing.T) {
	f := testFTPAgent(t)
	for i := 0; i < ftpWatchLimit+10; i++ {
		dir, err := os.MkdirTemp(f.root, "dir-")
		if err != nil {
			t.Fatal(err)
		}
		f.notifications.ensure(dir)
	}
	n := f.notifications
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.dirs) != ftpWatchLimit || n.lru.Len() != ftpWatchLimit {
		t.Fatalf("unbounded subscriptions: %d/%d", len(n.dirs), n.lru.Len())
	}
	if n.closed.Load() {
		t.Fatal("watch eviction closed notification stream")
	}
	for _, e := range n.dirs {
		if time.Until(e.Value.(ftpWatchEntry).expires) < 20*time.Second {
			t.Fatal("watch registration failed within budget")
		}
	}
}

func TestFTPNotifyVersionMismatch(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan error, 1)
	go func() { done <- ServeAgent(a, "test-release") }()
	if err := writeMessage(b, message{Type: "mount_mux", SoftwareVersion: "different-release"}); err != nil {
		t.Fatal(err)
	}
	m, err := readMessage(b)
	if err != nil || m.Type != "error" || !strings.Contains(m.Error, "version mismatch") {
		t.Fatalf("%+v %v", m, err)
	}
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("accepted mismatched version")
		}
	case <-time.After(time.Second):
		t.Fatal("agent stuck")
	}
}

func TestFTPNotifySFTPExitStopsAgent(t *testing.T) {
	if _, err := ftpSFTPServerExecutable(); err != nil {
		t.Skip(err)
	}
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan error, 1)
	go func() { done <- ServeAgent(a, "test-release") }()
	mux, data, _, err := OpenMux(b, "test-release")
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	// OpenSSH rejects this oversized packet and exits while both streams remain
	// open. The agent must reap the child without waiting for another client read.
	if _, err = data.Write([]byte{255, 255, 255, 255, 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent stuck after SFTP child exit")
	}
}

func TestFTPNotifyPreservesUnchangedSibling(t *testing.T) {
	f := testFTPAgent(t)
	for _, name := range []string{"left", "right"} {
		if err := os.Mkdir(filepath.Join(f.root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	left, right := filepath.Join(f.root, "left"), filepath.Join(f.root, "right")
	f.notifications.ensure(left)
	f.notifications.ensure(right)
	if _, err := f.readDirectory(left); err != nil {
		t.Fatal(err)
	}
	if _, err := f.readDirectory(right); err != nil {
		t.Fatal(err)
	}
	before := ftpInvalidations(f)
	if err := os.WriteFile(filepath.Join(left, "new"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitFTPInvalidation(t, f, before)
	f.cache.mu.Lock()
	cached := f.cache.entries["d:"+right] != nil
	stale := f.cache.entries["d:"+left] != nil
	f.cache.mu.Unlock()
	if !cached || stale {
		t.Fatalf("right cached=%v left stale=%v", cached, stale)
	}
}

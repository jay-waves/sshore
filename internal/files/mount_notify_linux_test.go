//go:build linux

package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/tests/testutil"
	sftp "github.com/pkg/sftp/v2"
)

func testFTPAgent(t *testing.T) *ftpFS {
	t.Helper()
	open := func(mode string) io.ReadWriteCloser {
		a, b := testutil.TCPPair(t)
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
		endpoint := newMountStream(b)
		if err := negotiateMount(endpoint, mode, "test-release", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		return endpoint
	}
	data, events := open("mount_data"), open("mount_watch")
	client, err := sftp.NewClientPipe(t.Context(), data, data)
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
	f.notifications.ensure(context.Background(), f.root)
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
				if !errors.Is(err, os.ErrNotExist) {
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
	f.notifications.ensure(context.Background(), sub)
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
		f.notifications.ensure(context.Background(), dir)
	}
	n := f.notifications
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.dirs) != ftpWatchLimit {
		t.Fatalf("unexpected subscription count: %d", len(n.dirs))
	}
	for _, entry := range n.dirs {
		if time.Until(entry.expires) < 20*time.Second {
			t.Fatal("watch registration failed within budget")
		}
	}
}

func TestFTPNotifyVersionMismatch(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan error, 1)
	go func() { done <- ServeAgent(a, "test-release") }()
	if err := writeMessage(b, message{Type: "mount_watch", SoftwareVersion: "different-release"}); err != nil {
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

func TestMountAgentRejectsOversizedSFTPHandshake(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan error, 1)
	go func() { done <- ServeAgent(a, "test-release") }()
	data := newMountStream(b)
	if err := negotiateMount(data, "mount_data", "test-release", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// The built-in server rejects oversized packets before reading their bodies.
	if _, err := data.Write([]byte{255, 255, 255, 255, 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted oversized handshake")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent stuck after oversized handshake")
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
	f.notifications.ensure(context.Background(), left)
	f.notifications.ensure(context.Background(), right)
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
	cached := f.cache.entries[ftpCacheKey{kind: ftpDirectoryKey, name: right}] != nil
	stale := f.cache.entries[ftpCacheKey{kind: ftpDirectoryKey, name: left}] != nil
	f.cache.mu.Unlock()
	if !cached || stale {
		t.Fatalf("right cached=%v left stale=%v", cached, stale)
	}
}

func TestMountApprovalSourceGate(t *testing.T) {
	a, b := net.Pipe()
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	defer b.Close()
	called := make(chan MountApproval, 1)
	done := make(chan error, 1)
	go func() {
		done <- ServeAgent(a, "1.2.3", func(source MountApproval) error { called <- source; return fmt.Errorf("source changed") })
	}()
	approval := &MountApproval{Session: "work", PID: 123, ID: strings.Repeat("a", 32)}
	if err := negotiateMount(b, "mount_watch", "1.2.3", approval, nil, nil); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("gate bypassed: %v", err)
	}
	if got := <-called; got != *approval {
		t.Fatal(got)
	}
	if err := <-done; err == nil {
		t.Fatal("agent accepted failed approval")
	}
}

func TestMountAgentPeerMetadataLifetime(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	peer := json.RawMessage(`{"id":"mount/example"}`)
	observed := make(chan string, 1)
	ended := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeAgentWithPeer(a, "test-release", "fixture", func(data json.RawMessage) func() { observed <- string(data); return func() { close(ended) } })
	}()
	var account string
	err := negotiateMount(b, "mount_data", "test-release", nil, peer, &account)
	if account != "fixture" {
		t.Fatalf("remote account = %q", account)
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-observed:
		if got != string(peer) {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("metadata not observed")
	}
	b.Close()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("peer registration outlived agent")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("agent did not exit")
	}
}

// Independent endpoints must not couple notification lifetime to SFTP I/O.
func TestMountEndpointsCloseIndependently(t *testing.T) {
	open := func(mode string) net.Conn {
		server, client := net.Pipe()
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		done := make(chan struct{})
		go func() { defer close(done); _ = ServeAgent(server, "test-release") }()
		t.Cleanup(func() {
			_ = client.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("mount endpoint did not exit")
			}
		})
		if err := negotiateMount(client, mode, "test-release", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		return client
	}
	data := open("mount_data")
	events := open("mount_watch")
	client, err := sftp.NewClientPipe(t.Context(), data, data)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = events.Close()
	root := t.TempDir()
	filename := filepath.Join(root, "after-notification-close")
	if err := os.WriteFile(filename, []byte("still connected"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := client.OpenFile(filename, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "still connected" {
		t.Fatalf("SFTP after notification close: %q, %v", got, err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestMountAgentClosesOpenHandlesOnDisconnect(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan error, 1)
	go func() { done <- ServeAgent(a, "test-release") }()
	data := newMountStream(b)
	if err := negotiateMount(data, "mount_data", "test-release", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	client, err := sftp.NewClientPipe(t.Context(), data, data)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filename := filepath.Join(root, "left-open")
	if _, err := client.Create(filename); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mount agent did not exit")
	}
	descriptors, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range descriptors {
		if name, _ := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); name == filename {
			t.Fatalf("mount agent left descriptor %s open for %s", fd.Name(), name)
		}
	}
}

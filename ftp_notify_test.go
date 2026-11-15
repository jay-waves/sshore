package main

import (
	"github.com/xtaci/smux"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFTPNotifyOptions(t *testing.T) {
	o, err := parseFTPOptions([]string{"host", "--server", "/a path/sshore", "--", "-J", "jump"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(mountSSHArgs(o), " ")
	if o.noWatch || !strings.Contains(args, "mount-agent") || !strings.Contains(args, "-J jump") || strings.Contains(args, "SessionType=subsystem") {
		t.Fatalf("%+v %s", o, args)
	}
	o.noWatch = true
	args = strings.Join(mountSSHArgs(o), " ")
	if !strings.HasSuffix(args, "-s -- host sftp") || strings.Contains(args, "mount-agent") {
		t.Fatal(args)
	}
}

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

func TestFTPNotifyRecovery(t *testing.T) {
	t.Setenv("SSHORE_DISABLED", "1")
	if _, err := run([]string{"mount-agent"}); err == nil {
		t.Fatal("mount agent allowed in recovery")
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
	go func() { n.ensure("/"); close(done) }()
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
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != 0 || n.generation.Load() != 1 {
		t.Fatal("reset did not invalidate cached metadata and subscriptions")
	}
}

//go:build !sshoretest

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonStateRootFollowsOnlyLiveOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "daemon")
	requested, active := t.TempDir(), t.TempDir()
	if root, err := daemonStateRootAt(dir, requested, false); err != nil || root != requested {
		t.Fatalf("missing daemon changed scope: %s %v", root, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("path lookup created daemon state")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := lockResourceState(owner); err != nil {
		t.Fatal(err)
	}
	record := localShellRecord{Name: "daemon", Version: version, Token: resourceToken(), Address: "127.0.0.1:1234", Directory: active}
	if err := writeStateJSON(filepath.Join(dir, "daemon.json"), record); err != nil {
		t.Fatal(err)
	}
	if root, err := daemonStateRootAt(dir, requested, false); err != nil || root != active {
		t.Fatalf("default client lost live scope: %s %v", root, err)
	}
	if _, err := daemonStateRootAt(dir, requested, true); err == nil {
		t.Fatal("conflicting override accepted")
	}
	if root, err := daemonStateRootAt(dir, active, true); err != nil || root != active {
		t.Fatalf("same override rejected: %s %v", root, err)
	}
	owner.Close()
	if root, err := daemonStateRootAt(dir, requested, false); err != nil || root != requested {
		t.Fatalf("stale locator pinned scope: %s %v", root, err)
	}
}

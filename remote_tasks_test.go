package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testPeerTask() remoteTask {
	return remoteTask{Version: version, Key: strings.Repeat("a", 32), ID: "forward/web", Manager: "test-client", State: "active", Detail: "server 8080 -> manager 3000"}
}
func TestPeerTaskPrivacyAndStorage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	task := testPeerTask()
	if e := storeRemoteTask(task); e != nil {
		t.Fatal(e)
	}
	dir, e := peerTaskDir(false)
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, task.Key+".json")
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	var saved remoteTask
	if e = json.Unmarshal(b, &saved); e != nil || saved != task {
		t.Fatal(saved, e)
	}
	if e = removeRemoteTask("../outside"); e == nil {
		t.Fatal("accepted invalid key")
	}
	if e = removeRemoteTask(task.Key); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(p); !os.IsNotExist(e) {
		t.Fatal("record survived removal", e)
	}
	s := &resourceService{record: resourceRecord{Kind: "mount", Name: "work", Remote: "/project", Mountpoint: "/private/local/path", Address: "127.0.0.1:9999", Token: "secret-token", Host: "private-alias"}}
	b, _ = json.Marshal(s.peerTask())
	for _, secret := range []string{"/private/local/path", "secret-token", "9999", "private-alias"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("private data published", string(b))
		}
	}
	task.Version = "older"
	if e = storeRemoteTask(task); e == nil {
		t.Fatal("accepted incompatible version")
	}
}
func TestSFTPPeerTaskSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("in-process SFTP fixture requires Unix permission modes")
	}
	backend := testFTPBackend(t)
	dir := filepath.Join(backend.root, "tasks")
	task := testPeerTask()
	if e := publishSFTPTask(backend.client, filepath.ToSlash(dir), task); e != nil {
		t.Fatal(e)
	}
	task.Detail = "updated"
	if e := publishSFTPTask(backend.client, filepath.ToSlash(dir), task); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(dir, task.Key+".json"))
	if e != nil {
		t.Fatal(e)
	}
	var saved remoteTask
	if json.Unmarshal(b, &saved) != nil || saved.Detail != "updated" {
		t.Fatal(string(b))
	}
	if e = os.Chmod(dir, 0755); e != nil {
		t.Fatal(e)
	}
	// Windows enforces private ACLs differently; this SFTP fixture is mode-based.
	if runtime.GOOS != "windows" {
		if e := publishSFTPTask(backend.client, filepath.ToSlash(dir), task); e == nil {
			t.Fatal("accepted public registry")
		}
	}
}
func TestPeerTaskLeaseStates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	task := testPeerTask()
	if e := storeRemoteTask(task); e != nil {
		t.Fatal(e)
	}
	dir, _ := peerTaskDir(false)
	p := filepath.Join(dir, task.Key+".json")
	var out bytes.Buffer
	if e := printRemoteTasks(&out); e != nil || !strings.Contains(out.String(), "active") || !strings.Contains(out.String(), "peer/aaaaaaaa/forward/web") {
		t.Fatal(out.String(), e)
	}
	stale := time.Now().Add(-2 * time.Minute)
	if e := os.Chtimes(p, stale, stale); e != nil {
		t.Fatal(e)
	}
	out.Reset()
	if e := printRemoteTasks(&out); e != nil || !strings.Contains(out.String(), "unknown") {
		t.Fatal(out.String(), e)
	}
	old := time.Now().Add(-25 * time.Hour)
	if e := os.Chtimes(p, old, old); e != nil {
		t.Fatal(e)
	}
	if e := listRemoteTasks(); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(p); !os.IsNotExist(e) {
		t.Fatal("expired lease retained")
	}
}

//go:build sshoretest

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResetEndsTasksAndClearsState(t *testing.T) {
	e := newEnv(t)
	shell, _ := e.attach("reset-hosted")
	defer shell.close()
	root, err := statePath("")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "shells", "record")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("history"), 0600); err != nil {
		t.Fatal(err)
	}
	cache, err := resetCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "cached"), []byte("cache"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(root, "user-file")
	if err := os.WriteFile(unrelated, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(e.bin, "reset")
	cmd.Env = append(e.env, "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reset: %s %v", out, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("history remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "cached")); !os.IsNotExist(err) {
		t.Fatalf("cache remains: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("user file lost: %v", err)
	}
	if _, err := daemonAdmin(context.Background(), "snapshot", nil); !os.IsNotExist(err) {
		t.Fatalf("daemon remains: %v", err)
	}
}

func TestResetRetainsStateWhenShutdownRefused(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	task, _ := registerTestResource(t, tasks, "mount", "busy", strings.Repeat("a", 32))
	task.control = func(string) resourceReply { return resourceReply{Error: "mount is busy"} }
	root, err := stateDir("history")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "record")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runReset(); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("refusal: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("state lost on refusal: %v", err)
	}
}

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	statefs "github.com/jay-waves/sshore/internal/state"
)

func TestListScopes(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("SSHORE_STATE_DIR", state)
	dir, err := resourceStateDir("forward")
	if err != nil {
		t.Fatal(err)
	}
	if err = statefs.WriteJSON(filepath.Join(dir, "local-only.json"), resourceRecord{Kind: "forward", Name: "local-only", Host: "work", Version: version}); err != nil {
		t.Fatal(err)
	}
	if err = saveShellSnapshot(shellSnapshot{Host: "work", Name: "cached", State: "detached", ConfirmedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "ssh-called")
	wrapper := filepath.Join(root, "ssh")
	if err = os.WriteFile(wrapper, []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\nprintf '%s\\n' "+shellQuote(`{"version":"`+version+`","resources":[],"shells":[{"name":"remote-only","state":"detached"}],"peers":[],"errors":[]}`)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (inventory, string, error) {
		cmd := exec.Command(testBinary(t), args...)
		cmd.Env = append(os.Environ(), "SSHORE_SSH="+wrapper, "SSHORE_SOCKET=", "SSH_CONNECTION=", "SSHORE_RUNTIME_DIR="+filepath.Join(root, "runtime"), "HOME="+root)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		inv := emptyInventory()
		if err == nil {
			err = json.Unmarshal(out, &inv)
		}
		return inv, stderr.String(), err
	}
	local, _, err := run("list", "--json")
	if err != nil || local.RemoteQueried || len(local.Resources) != 1 || len(local.Shells) != 1 {
		t.Fatalf("%+v %v", local, err)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("offline list invoked SSH")
	}
	remote, stderr, err := run("list", "work", "--json")
	if err != nil || !remote.RemoteQueried || len(remote.Resources) != 0 || len(remote.Shells) != 1 || remote.Shells[0].Name != "remote-only" || stderr != "" {
		t.Fatalf("%+v %s %v", remote, stderr, err)
	}
	if err = os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = run("list", "work", "--json")
	if err == nil || !strings.Contains(stderr, "remote query failed") {
		t.Fatalf("%s %v", stderr, err)
	}
}

func TestInventoryOmitsExpiredExitReceipts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	dir, err := runtimeDir()
	if err != nil {
		t.Fatal(err)
	}
	for name, age := range map[string]time.Duration{"recent": time.Hour, "expired": 8 * 24 * time.Hour} {
		if err := statefs.WriteJSON(filepath.Join(dir, name+".sock.exit.json"), sessionExit{Name: name, Reason: "exited", At: time.Now().Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := localSessionInventory()
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Exits) != 1 || inv.Exits[0].Name != "recent" {
		t.Fatalf("exit receipts: %#v", inv.Exits)
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMetadataPublication(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, err := stateDir("records")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "record.json")
	if err := writeStateJSON(path, map[string]int{"value": 1}); err != nil {
		t.Fatal(err)
	}
	if err := privateStatePath(path, false); err != nil {
		t.Fatal(err)
	}
	if err := writeStateJSON(path, make(chan int)); err == nil {
		t.Fatal("accepted non-JSON metadata")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"value":1}` {
		t.Fatalf("failed encoding damaged record: %q %v", data, err)
	}
	if err := writeStateJSON(path, map[string]int{"value": 2}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != `{"value":2}` {
		t.Fatalf("replacement: %q %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v %v", entries, err)
	}
	if err := writeStateFile(filepath.Join(dir, "missing", "record"), []byte("x")); err == nil {
		t.Fatal("accepted missing parent")
	}
}

func TestIdentityResetAndRoutes(t *testing.T) {
	dirRoot := t.TempDir()
	if err := os.Chmod(dirRoot, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_STATE_DIR", dirRoot)
	first, err := persistentMachineIdentity(false)
	if err != nil || !validMachineID(first) {
		t.Fatalf("identity: %q %v", first, err)
	}
	again, err := persistentMachineIdentity(false)
	if err != nil || again != first {
		t.Fatal("identity changed", err)
	}
	for i, host := range []string{"host-a", "host-a", "host-b", "host-b"} {
		warn, err := observeIdentityRoute(first, host, nil)
		if err != nil || warn != (i == 2) {
			t.Fatalf("route %d: %v %v", i, warn, err)
		}
	}
	dir, _ := stateDir("connections")
	path := filepath.Join(dir, "recent.json")
	if err = os.WriteFile(path, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = runReset(); err != nil {
		t.Fatal(err)
	}
	next, err := persistentMachineIdentity(false)
	if err != nil || next == first {
		t.Fatal("reset did not change identity", err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("history retained", err)
	}
	warn, err := observeIdentityRoute(first, "host-c", nil)
	if err != nil || warn {
		t.Fatal("route history retained", err)
	}
}

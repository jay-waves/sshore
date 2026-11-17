package main

import (
	"os"
	"path/filepath"
	"sync"
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

func TestPendingRequestOwnsName(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	startRequestDaemon(t)
	id, err := queueRequest(testRequestOptions(), []operation{{Kind: "forward", TargetPort: 3000}})
	if err != nil {
		t.Fatal(err)
	}
	name := reviewName(t, id)
	for _, kind := range []string{"mount", "forward", "shell"} {
		if _, err := chooseTaskIdentity(kind, name); err == nil {
			t.Fatalf("%s stole pending request name", kind)
		}
	}
	if _, err := chooseTaskIdentityExcept("review", name, id); err != nil {
		t.Fatal(err)
	}
	if err := consumeRequest(id); err != nil {
		t.Fatal(err)
	}
	if _, err := chooseTaskIdentity("mount", name); err != nil {
		t.Fatal(err)
	}
}

func TestSessionNames(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range automaticSessionNames() {
		if err := validSession(name); err != nil || seen[name] {
			t.Fatalf("invalid or duplicate name %q", name)
		}
		seen[name] = true
	}
	if len(seen) == 0 {
		t.Fatal("missing automatic IDs")
	}
}

func TestAcceptSessionName(t *testing.T) {
	for _, tc := range []struct {
		label      string
		opts       options
		name, want string
		wantError  bool
	}{
		{"automatic", options{}, "pikachu", "pikachu", false},
		{"invalid remote name", options{}, "../bad", "", true},
		{"missing remote name", options{}, "", "", true},
		{"explicit", options{session: "build"}, "", "build", false},
		{"recovery has no shell", options{plain: true}, ".", "", false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			err := acceptSessionName(&tc.opts, tc.name)
			if (err != nil) != tc.wantError || tc.opts.session != tc.want {
				t.Fatalf("shell %q, error %v", tc.opts.session, err)
			}
		})
	}
}

func TestTaskNamesClaimedAtomicallyByDaemon(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	tasks := startRequestDaemon(t)
	var wg sync.WaitGroup
	wins := make(chan string, 32)
	done := make(chan struct{})
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := newTaskUID()
			if err != nil {
				t.Error(err)
				return
			}
			task := &daemonResourceTask{record: resourceRecord{UID: id, Name: "shared", Kind: "forward"}, done: done}
			if tasks.register(id, task) == nil {
				wins <- id
			}
		}()
	}
	wg.Wait()
	if len(wins) != 1 {
		t.Fatalf("concurrent name owners: %d", len(wins))
	}
	if _, err := chooseTaskIdentity("mount", "shared"); err == nil {
		t.Fatal("selected occupied name")
	}
	close(done)
	next, err := chooseTaskIdentity("mount", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if next.uid == <-wins {
		t.Fatal("reused old task identity")
	}
	if _, err := os.Stat(filepath.Join(root, "names")); !os.IsNotExist(err) {
		t.Fatalf("name selection created files: %v", err)
	}
}

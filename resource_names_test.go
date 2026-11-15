package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestResourceNameReservations(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	names := automaticSessionNames()
	reservations := make(chan *resourceNameReservation, len(names))
	var wg sync.WaitGroup
	for range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := reserveResourceName("forward", "")
			if err != nil {
				t.Error(err)
				return
			}
			reservations <- r
		}()
	}
	wg.Wait()
	close(reservations)
	seen := map[string]bool{}
	for r := range reservations {
		defer r.Close()
		if seen[r.name] {
			t.Fatalf("duplicate %s", r.name)
		}
		seen[r.name] = true
	}
	if len(seen) != len(names) {
		t.Fatal(seen)
	}
	if _, err := reserveResourceName("forward", ""); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatal(err)
	}
	if _, err := reserveResourceName("forward", names[0]); err == nil {
		t.Fatal("accepted occupied explicit name")
	}
	for _, tc := range []struct{ kind, name string }{{"mount", "custom-mount"}, {"forward", "custom"}} {
		r, err := reserveResourceName(tc.kind, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
	}
}

func TestResourceNameHandoff(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	r, err := reserveResourceName("mount", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dir, _ := stateDir("names")
	r.owner.Close()
	r.owner = nil
	if _, err := reserveResourceName("mount", r.name); err == nil {
		t.Fatal("startup handoff lost reservation")
	}
	worker, err := os.OpenFile(filepath.Join(dir, r.name+".lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if err := lockResourceState(worker); err != nil {
		t.Fatal(err)
	}
	r.Close()
	next, err := reserveResourceName("mount", "")
	if err != nil {
		t.Fatal(err)
	}
	if next.name == r.name {
		t.Fatal("reused live worker name")
	}
	next.Close()
	worker.Close()
	reused, err := reserveResourceName("mount", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Close()
	if reused.name != r.name {
		t.Fatalf("did not reuse released name: %s", reused.name)
	}
}

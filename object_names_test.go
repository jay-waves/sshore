package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	statefs "github.com/jay-waves/sshore/internal/state"
)

func TestReviewNameReservedForAction(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	id, err := writeApproval(options{host: "work", session: "main", server: "sshore"}, []operation{{Kind: "forward", TargetPort: 3000}})
	if err != nil {
		t.Fatal(err)
	}
	name := reviewName(t, id)
	for _, kind := range []string{"mount", "forward", "shell"} {
		if r, err := reserveResourceName(kind, name); err == nil {
			r.Close()
			t.Fatalf("%s stole pending review name", kind)
		}
	}
	dir, _ := approvalDir()
	request, err := findApproval(dir, name)
	if err != nil || request.ID != id {
		t.Fatalf("%+v %v", request, err)
	}
	reservation, err := reserveResourceNameExcept("review", name, id)
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Close()
	if err := consumeApproval(dir, id); err != nil {
		t.Fatal(err)
	}
	if r, err := reserveResourceName("forward", name); err == nil {
		r.Close()
		t.Fatal("name reused during approval handoff")
	}
	reservation.Close()
	r, err := reserveResourceName("mount", name)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
}

func TestSessionNames(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range automaticSessionNames() {
		if err := validSession(name); err != nil || seen[name] {
			t.Fatalf("invalid or duplicate name %q", name)
		}
		seen[name] = true
	}
	if len(seen) != 154 {
		t.Fatal("expected 154 automatic names")
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
	dir, _ := statefs.Dir("names")
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

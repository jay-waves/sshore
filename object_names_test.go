package main

import (
	"path/filepath"
	"testing"
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

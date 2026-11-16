package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResourceIdentityAndLegacyMount(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, e := resourceStateDir("mount")
	if e != nil {
		t.Fatal(e)
	}
	// Existing private mount records continue to work without a Kind field.
	b, _ := json.Marshal(resourceRecord{Name: "work", Host: "work", Version: version})
	if e = os.WriteFile(filepath.Join(dir, "work.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	r, e := readResourceRecord("mount", "work")
	if e != nil || r.Kind != "mount" {
		t.Fatal(r, e)
	}
	for _, id := range []string{"work", "shell/work", "mount/../work", "review/abc"} {
		if _, e := runCancel([]string{id}); e == nil {
			t.Fatal("accepted", id)
		}
	}
	id := strings.Repeat("a", 32)
	o, e := parseReview([]string{"review/" + id})
	if e != nil || o.id != id {
		t.Fatal(o, e)
	}
}

func TestCancelPendingRequest(t *testing.T) {
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "nonexistent-ssh"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	id, err := writeApproval(options{host: "fixture", server: "sshore", session: "work"}, []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"cancel", "review/" + id, "extra"}, {"review", id, "--reject"}} {
		if _, err := run(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	dir, err := approvalDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readApproval(dir, id); err != nil {
		t.Fatal("invalid invocation consumed request", err)
	}
	if code, err := run([]string{"cancel", "review/" + id}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := run([]string{"cancel", "review/" + id}); err == nil {
		t.Fatal("cancelled twice")
	}
	for _, args := range [][]string{{"ftp", "host"}, {"approve"}, {"stop", "forward/x"}, {"kill", "host", "work"}, {"cancel"}, {"cancel", "host"}, {"cancel", "peer/forward/x"}, {"review", "--list"}, {"mount", "--list"}, {"forward", "--list"}} {
		if _, err := run(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestApprovalLifecycle(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	ops := []operation{{Kind: "download", Path: "/tmp/a", Name: "a", Size: 12}}
	id, err := writeApproval(options{host: "host", session: "main", server: "sshore", ssh: []string{"-p", "2222"}}, ops)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := approvalDir()
	r, err := readApproval(dir, id)
	if err != nil || r.Operations[0].Path != "/tmp/a" || r.SSH[1] != "2222" {
		t.Fatal(r, err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if consumeApproval(dir, id) == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("approval executed more than once", successes.Load())
	}
	if _, err = readApproval(dir, id); !os.IsNotExist(err) {
		t.Fatal("consumed request survived", err)
	}
	id, err = writeApproval(options{host: "host", session: "main", server: "sshore"}, ops)
	if err != nil {
		t.Fatal(err)
	}
	r, _ = readApproval(dir, id)
	r.Created = time.Now().Add(-2 * approvalTTL)
	data, _ := json.Marshal(r)
	if err = os.WriteFile(filepath.Join(dir, id+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readApproval(dir, id); err == nil {
		t.Fatal("expired approval accepted")
	}
}
func TestApproveCLI(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, args := range [][]string{{}, {"--stay"}, {id, "--stay"}} {
		if _, err := parseReview(args); err != nil {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{{"--attach"}, {"--list"}, {"--stay", "--attach"}, {"--list", "--stay"}, {"--reject"}, {"../bad"}, {id, id}, {id, "--reject", "--attach"}} {
		if _, err := parseReview(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	o, _ := parseReview(nil)
	if o.stay {
		t.Fatal("default should attach")
	}
	o, _ = parseReview([]string{"--stay"})
	if !o.stay {
		t.Fatal("stay ignored")
	}
}

func TestApprovalRejectsDifferentRelease(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	id, err := writeApproval(options{host: "host", session: "main", server: "sshore"}, []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := approvalDir()
	if err != nil {
		t.Fatal(err)
	}
	r, err := readApproval(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	series, _ := releaseSeries(version)
	r.SoftwareVersion = series + ".999"
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readApproval(dir, id); err == nil {
		t.Fatal("different patch approval accepted")
	}
	for _, release := range []string{"0.0.0-old", ""} {
		r.SoftwareVersion = release
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readApproval(dir, id); err == nil || !strings.Contains(err.Error(), "incompatible") {
			t.Fatal("accepted incompatible approval", err)
		}
	}
}

func reviewName(t *testing.T, id string) string {
	t.Helper()
	dir, err := approvalDir()
	if err != nil {
		t.Fatal(err)
	}
	r, err := readApproval(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	return r.DisplayName()
}

func TestApprovalValidation(t *testing.T) {
	for _, ops := range [][]operation{
		nil, {{Kind: "mount", Path: "relative"}}, {{Kind: "mount", Path: "/tmp", Port: 80}},
		{{Kind: "mount", Path: "/tmp"}, {Kind: "upload", Path: "/tmp"}},
		{{Kind: "upload", Path: "/tmp", ReadOnly: true}}, {{Kind: "exec", Path: "/bin/sh"}}, {{Kind: "download", Path: "/tmp/a", Name: "../a", Size: 1}},
		{{Kind: "download", Path: "relative", Name: "a", Size: 1}}, {{Kind: "download", Path: "/a", Name: "a", Size: -1}},
		{{Kind: "forward", Port: 80, Reverse: true, TargetPort: -1}},
		{{Kind: "upload", Path: "/tmp", Reverse: true}},
		{{Kind: "forward", Port: 0}}, {{Kind: "forward", Port: 80, Path: "/a"}},
		{{Kind: "upload", Path: "/tmp", Name: "secret"}},
	} {
		if validateApprovalOperations(ops) == nil {
			t.Fatal("accepted", ops)
		}
	}
	if err := validateApprovalOperations([]operation{{Kind: "forward", Port: 7777, TargetPort: 6666}}); err != nil {
		t.Fatal("valid remote target rejected:", err)
	}
	if err := validateApprovalOperations([]operation{{Kind: "mount", Path: "/tmp", ReadOnly: true}}); err != nil {
		t.Fatal(err)
	}
	// A malicious peer cannot deserialize a chosen local path.
	var op operation
	if err := json.Unmarshal([]byte(`{"kind":"upload","path":"/tmp","LocalPath":"/etc/passwd"}`), &op); err != nil {
		t.Fatal(err)
	}
	if op.LocalPath != "" {
		t.Fatal("remote selected local path")
	}
}

func TestRoutedApprovalRecovery(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	parent := options{host: "entry", session: "parent", instance: "parent-instance", server: "/custom/sshore", ssh: []string{"-p", "2222"}}
	original := options{host: "private-alias", session: "child", instance: "child-instance", routeParent: &parent, routeToken: strings.Repeat("a", 64), routeTarget: true, resumePID: 123}
	id, err := writeApproval(original, []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := approvalDir()
	r, err := readApproval(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	restored := r.clientOptions()
	if restored.command != "attach" || restored.routeParent == nil || restored.routeParent.host != "entry" || restored.routeParent.instance != parent.instance || restored.instance != original.instance || restored.routeToken != original.routeToken || !restored.routeTarget || restored.resumePID != 123 || restored.approvalID != id {
		t.Fatalf("approval lost route: %+v", restored)
	}
	r.Parent.Instance = ""
	if err := validateApprovalRecord(r); err == nil {
		t.Fatal("unbound approval route accepted")
	}
}

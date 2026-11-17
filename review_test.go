package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func testRequestOptions() options {
	return options{host: "work", session: "source", server: "sshore", instance: strings.Repeat("a", 32), shellPID: 123}
}

func reviewName(t *testing.T, id string) string {
	t.Helper()
	r, err := readRequest(id)
	if err != nil {
		t.Fatal(err)
	}
	return r.DisplayName()
}

func TestRequestSingleConsumption(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	startRequestDaemon(t)
	id, err := queueRequest(testRequestOptions(), []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := readRequest(id)
	if err != nil || r.ID != id {
		t.Fatal(r, err)
	}
	r.Operations[0].LocalPath = "private selection"
	original, err := readRequest(id)
	if err != nil || original.Operations[0].LocalPath != "" {
		t.Fatal("selection changed pending intent", err)
	}
	var count atomic.Int32
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if consumeRequest(id) == nil {
				count.Add(1)
			}
		})
	}
	workers.Wait()
	if count.Load() != 1 {
		t.Fatalf("consumed %d times", count.Load())
	}
	if _, err := readRequest(id); err == nil {
		t.Fatal("consumed request remained")
	}
	root, _ := statePath("")
	if _, err := os.Stat(filepath.Join(root, "approvals")); !os.IsNotExist(err) {
		t.Fatal("request state persisted", err)
	}
}

func TestRequestSeparatesLocalDefaultsFromConnectionDirectory(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	startRequestDaemon(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	o := testRequestOptions()
	o.sshDirectory = t.TempDir()
	o.ssh = []string{"-F", "ssh.conf"}
	id, err := queueRequest(o, []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := readRequest(id)
	if err != nil || r.Directory != cwd || r.Connection.Directory != o.sshDirectory {
		t.Fatal(r, err)
	}
}

func TestRequestPreservesHostedConnection(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	startRequestDaemon(t)
	o := testRequestOptions()
	o.local = true
	o.hosted = true
	o.host = "local"
	id, err := queueRequest(o, []operation{{Kind: "forward", Port: 1234, TargetPort: 2345}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := readRequest(id)
	if err != nil || !r.Connection.Hosted || r.Connection.Instance != o.instance {
		t.Fatal(r, err)
	}
	r.Connection.Host = "remote"
	if validateRequestRecord(r) == nil {
		t.Fatal("hosted request accepted an SSH target")
	}
}

func TestReviewCLI(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, args := range [][]string{nil, {id}} {
		if _, err := parseReview(args); err != nil {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{{"--stay"}, {"--attach"}, {"--list"}, {"--reject"}, {"../bad"}, {id, id}} {
		if _, err := parseReview(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
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
		if validateRequestOperations(ops) == nil {
			t.Fatal("accepted", ops)
		}
	}
	if err := validateRequestOperations([]operation{{Kind: "forward", Port: 7777, TargetPort: 6666}}); err != nil {
		t.Fatal("valid remote target rejected:", err)
	}
	if err := validateRequestOperations([]operation{{Kind: "mount", Path: "/tmp", ReadOnly: true}}); err != nil {
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

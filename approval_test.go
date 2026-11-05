package main

import (
	"context"
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
func TestApprovalValidation(t *testing.T) {
	for _, ops := range [][]operation{
		nil, {{Kind: "exec", Path: "/bin/sh"}}, {{Kind: "download", Path: "/tmp/a", Name: "../a", Size: 1}},
		{{Kind: "download", Path: "relative", Name: "a", Size: 1}}, {{Kind: "download", Path: "/a", Name: "a", Size: -1}},
		{{Kind: "forward", Port: 0}}, {{Kind: "forward", Port: 80, Path: "/a"}},
		{{Kind: "upload", Path: "/tmp", Name: "secret"}},
	} {
		if validateOperations(ops) == nil {
			t.Fatal("accepted", ops)
		}
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
func TestApproveCLI(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, args := range [][]string{{}, {"--stay"}, {"--attach"}, {id, "--stay"}, {"--list"}, {id, "--reject"}} {
		if _, err := parseApprove(args); err != nil {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{{"--stay", "--attach"}, {"--list", "--stay"}, {"--reject"}, {"../bad"}, {id, id}, {id, "--reject", "--attach"}} {
		if _, err := parseApprove(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	o, _ := parseApprove(nil)
	if o.stay {
		t.Fatal("default should attach")
	}
	o, _ = parseApprove([]string{"--stay"})
	if !o.stay {
		t.Fatal("stay ignored")
	}
}
func TestOwnedSSHTransport(t *testing.T) {
	cmd := sshCommand(options{host: "host", server: "sshore", ssh: []string{"-o", "ControlMaster=auto", "-o", "ControlPath=/tmp/shared"}})
	args := strings.Join(cmd.Args, " ")
	if !strings.HasPrefix(args, "ssh -o ControlMaster=no -o ControlPath=none -o ControlPersist=no ") {
		t.Fatal(args)
	}
}
func TestApprovalMismatchDoesNotWriteFile(t *testing.T) {
	client, server := muxPair(t)
	target := filepath.Join(t.TempDir(), "secret")
	go func() {
		st, err := server.AcceptStream()
		if err != nil {
			return
		}
		defer st.Close()
		_, _ = readMessage(st)
		_ = writeMessage(st, message{Type: "offer", Name: "other", Size: 3})
	}()
	err := executeApprovedFile(context.Background(), client, operation{Kind: "download", Path: "/tmp/a", Name: "a", Size: 3, LocalPath: target})
	if err == nil {
		t.Fatal("mismatch accepted")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("wrote unapproved file", err)
	}
}

func TestChangedUploadIsNotSent(t *testing.T) {
	client, server := muxPair(t)
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	result := make(chan message, 1)
	go func() {
		st, err := server.AcceptStream()
		if err != nil {
			return
		}
		defer st.Close()
		_, _ = readMessage(st)
		_ = writeMessage(st, message{Type: "upload"})
		m, _ := readMessage(st)
		result <- m
	}()
	err := executeApprovedFile(context.Background(), client, operation{Kind: "upload", Path: "/tmp", Name: "source", Size: 1, LocalPath: path}, true)
	if err == nil {
		t.Fatal("changed upload accepted")
	}
	select {
	case m := <-result:
		if m.Type == "offer" || m.Type == "chunk" {
			t.Fatal("unapproved upload sent", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not close")
	}
}

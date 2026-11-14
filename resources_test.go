package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	for _, id := range []string{"work", "shell/work", "mount/../work", "approval/abc"} {
		if _, e := runCancel([]string{id}); e == nil {
			t.Fatal("accepted", id)
		}
	}
	id := strings.Repeat("a", 32)
	o, e := parseReview([]string{"approval/" + id})
	if e != nil || o.id != id {
		t.Fatal(o, e)
	}
}
func TestForwardTransportDoesNotReplayOtherRules(t *testing.T) {
	args := forwardSSHArgs(forwardConfig{Host: "work", Name: "web", Port: 8080, Target: 3000, SSH: []string{"-p", "2222", "-L", "9999:localhost:9999", "-R", "8888:localhost:8888", "-D", "7777"}}, true)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "9999") || strings.Contains(joined, "8888") || strings.Contains(joined, "7777") || !strings.Contains(joined, "-p 2222") || !strings.Contains(joined, "-L 127.0.0.1:8080:127.0.0.1:3000") {
		t.Fatal(args)
	}
}

func TestAttachmentRejectsTunnelStreams(t *testing.T) {
	client, server := muxPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveApprovedStreams(ctx, server)
	for _, kind := range []string{"forward_connect", "forward_control"} {
		st, e := client.OpenStream()
		if e != nil {
			t.Fatal(e)
		}
		st.SetDeadline(time.Now().Add(time.Second))
		if e = writeMessage(st, message{Type: kind, Action: "add"}); e != nil {
			t.Fatal(e)
		}
		if m, e := readMessage(st); e == nil {
			t.Fatalf("obsolete attachment tunnel accepted: %+v", m)
		}
		st.Close()
	}
}

func TestCancelPendingRequest(t *testing.T) {
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "nonexistent-ssh"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	id, err := writeApproval(options{host: "fixture", server: "sshore", session: "work"}, []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"cancel", "approval/" + id, "extra"}, {"review", id, "--reject"}} {
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
	if code, err := run([]string{"cancel", "approval/" + id}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := run([]string{"cancel", "approval/" + id}); err == nil {
		t.Fatal("cancelled twice")
	}
	for _, args := range [][]string{{"ftp", "host"}, {"approve"}, {"stop", "forward/x"}, {"kill", "host", "work"}, {"cancel"}, {"cancel", "host"}, {"cancel", "peer/forward/x"}, {"review", "--list"}, {"mount", "--list"}, {"forward", "--list"}} {
		if _, err := run(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

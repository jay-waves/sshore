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
	dir, e := ftpStateDir()
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
		if _, e := runStop([]string{id}); e == nil {
			t.Fatal("accepted", id)
		}
	}
	id := strings.Repeat("a", 32)
	o, e := parseApprove([]string{"approval/" + id, "--reject"})
	if e != nil || o.id != id || !o.reject {
		t.Fatal(o, e)
	}
}
func TestForwardTransportDoesNotReplayOtherRules(t *testing.T) {
	args := backgroundForwardArgs(forwardConfig{Host: "work", Name: "web", Port: 8080, Target: 3000, SSH: []string{"-p", "2222", "-L", "9999:localhost:9999", "-R", "8888:localhost:8888", "-D", "7777"}})
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
		if e = writeMessage(st, message{Type: kind, Action: "add", Port: 3000, RemotePort: 3001}); e != nil {
			t.Fatal(e)
		}
		if m, e := readMessage(st); e == nil {
			t.Fatalf("obsolete attachment tunnel accepted: %+v", m)
		}
		st.Close()
	}
}

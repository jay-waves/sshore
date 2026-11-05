package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApprovedTransferRequiresMatchingMetadata(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("main")
	source := filepath.Join(e.dir, "source")
	if err := os.WriteFile(source, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(e.dir, "target")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := executeApprovedFile(ctx, b.mux, operation{Kind: "download", Path: source, Name: "source", Size: 1, LocalPath: target}, true)
	if err == nil {
		t.Fatal("changed remote file accepted")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("unapproved file created", err)
	}
	// The same stream path succeeds for the exact approved metadata.
	if err = executeApprovedFile(ctx, b.mux, operation{Kind: "download", Path: source, Name: "source", Size: 7, LocalPath: target}, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "changed" {
		t.Fatal(string(data), err)
	}
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
}

func TestApprovalPrivateStateAndSymlinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := approvalDir(); err == nil {
		t.Fatal("public state directory accepted")
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := approvalDir()
	if err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	target := filepath.Join(t.TempDir(), "data")
	if err = os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, filepath.Join(dir, id+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err = readApproval(dir, id); err == nil {
		t.Fatal("symlink record accepted")
	}
}

func TestApprovedAttachRejectsChangedSession(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	sendTest(t, b.in, message{Type: "bye"})
	b.close()
	next := e.bridge("main")
	sendTest(t, next.in, message{Type: "attach", Version: protocolVersion, Action: "approved", PID: pid + 1, Term: "xterm", Cols: 80, Rows: 24})
	if m := recv(t, next.out); m.Type != "fatal" {
		t.Fatal("accepted wrong shell", m)
	}
	sendTest(t, next.in, message{Type: "bye"})
	next.close()
	resumed, resumedPID := e.attach("main")
	if resumedPID != pid {
		t.Fatal("shell was replaced")
	}
	sendTest(t, resumed.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, resumed.tty, "exit")
}

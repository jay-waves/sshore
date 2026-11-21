package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApprovalSourceRejectsReusedNameAndPID(t *testing.T) {
	s, request := routedResourceFixture(t)
	request.Instance = "replacement"
	if err := s.checkApprovalSource(request); err == nil {
		t.Fatal("approval accepted another instance with the same name and PID")
	}
	request.Instance = s.record.UID
	if err := s.checkApprovalSource(request); err != nil {
		t.Fatal(err)
	}
}

func TestApprovedTransferRequiresMatchingMetadata(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	id, err := resolveSession("main")
	if err != nil {
		t.Fatal(err)
	}
	detachTestClient(t, b)
	open := func() (net.Conn, error) {
		conn, err := e.dial("main")
		if err != nil {
			return nil, err
		}
		sendTest(t, conn, message{Type: "route_files", SoftwareVersion: version, Name: "main", PID: pid, Instance: id, ApprovalID: strings.Repeat("a", 32)})
		if reply := recv(t, conn); reply.Type != "resource_ready" {
			conn.Close()
			t.Fatalf("file route: %+v", reply)
		}
		return conn, nil
	}
	source := filepath.Join(e.dir, "source")
	if err := os.WriteFile(source, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(e.dir, "target")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = executeApprovedFile(ctx, open, newApprovedFile(operation{Kind: "download", Path: source, Name: "source", Size: 1}, target, nil), true)
	if err == nil {
		t.Fatal("changed remote file accepted")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("unapproved file created", err)
	}
	// The same stream path succeeds for the exact approved metadata.
	if err = executeApprovedFile(ctx, open, newApprovedFile(operation{Kind: "download", Path: source, Name: "source", Size: 7}, target, nil), true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "changed" {
		t.Fatal(string(data), err)
	}
	resumed, resumedPID := e.attach("main", "attach")
	if resumedPID != pid {
		t.Fatal("file execution replaced source shell")
	}
	resumed.writeInput(t, []byte("exit\n"))
	resumed.readExit(t)
}

func TestApprovedAttachRejectsChangedSession(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	sendTest(t, b.in, message{Type: "bye"})
	b.close()
	next := e.bridge("main", "attach")
	next.sendAttach(t, message{Type: "attach", Action: "existing", Instance: newTaskUID(), Term: "xterm", Cols: 80, Rows: 24})
	if m := recv(t, next.out); m.Type != "fatal" || !strings.Contains(m.Error, "shell changed") {
		t.Fatalf("stale shell instance: %+v", m)
	}
	sendTest(t, next.in, message{Type: "bye"})
	next.close()
	resumed, resumedPID := e.attach("main", "attach")
	if resumedPID != pid {
		t.Fatal("shell was replaced")
	}
	resumed.writeInput(t, []byte("exit\n"))
	resumed.readExit(t)
}

func TestEndpointRequestHelpers(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("main")
	sendTest(t, b.in, message{Type: "approval_ready"})
	source := filepath.Join(e.dir, "copy-source")
	if err := os.WriteFile(source, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args       []string
		kind, hint string
		reverse    bool
	}{
		{[]string{"cp", "origin:", ".:" + e.dir}, "upload", "", false},
		{[]string{"cp", "origin:/client/source", ".:" + e.dir}, "upload", "/client/source", false},
		{[]string{"cp", ".:" + source, "origin:/client/target"}, "download", "/client/target", false},
		{[]string{"cp", "::", ".:" + e.dir}, "upload", "", false},
		{[]string{"cp", ".:" + source, "::/client/target"}, "download", "/client/target", false},
		{[]string{"mount", ".:" + e.dir, "origin:"}, "mount", "", false},
		{[]string{"mount", ".:" + e.dir, "origin:/client/mount", "--read-only"}, "mount", "/client/mount", false},
		{[]string{"mount", ".:" + e.dir, "::"}, "mount", "", false},
		{[]string{"mount", ".:" + e.dir, "::/client/mount", "--read-only"}, "mount", "/client/mount", false},
		{[]string{"forward", "origin:", ".:3000"}, "forward", "", false},
		{[]string{"forward", ".:3000", "origin:8080"}, "forward", "8080", true},
		{[]string{"forward", "::", ".:3000"}, "forward", "", false},
		{[]string{"forward", ".:3000", "::8080"}, "forward", "8080", true},
	} {
		_, done := e.helper(tc.args[0], tc.args[1:]...)
		m := recv(t, b.out)
		if m.Type != "approval_request" || m.SoftwareVersion != version || len(m.Operations) != 1 {
			t.Fatal(m)
		}
		op := m.Operations[0]
		if op.Kind != tc.kind || op.LocalHint != tc.hint || op.Reverse != tc.reverse {
			t.Fatal(op)
		}
		if op.Kind == "mount" && (op.Path != e.dir || op.ReadOnly != (tc.hint != "")) {
			t.Fatal(op)
		}
		if op.Kind == "forward" && (!op.LocalPortPending() || op.ServerPort() != 3000) {
			t.Fatal("resolved without local consent", op)
		}
		sendTest(t, b.in, message{Type: "approval_handoff", Token: m.Token})
		waitHelper(t, done, true)
		if reply := recv(t, b.out); reply.Type != "approval_ack" || reply.Token != m.Token {
			t.Fatal(reply)
		}
	}
	b.writeInput(t, []byte("exit\n"))
	b.readExit(t)
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEndpointRequestHelpers(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("main")
	source := filepath.Join(e.dir, "copy-source")
	if err := os.WriteFile(source, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args       []string
		kind, hint string
		reverse    bool
	}{
		{[]string{"cp", ":", e.dir}, "upload", "", false},
		{[]string{"cp", "origin:/client/source", e.dir}, "upload", "/client/source", false},
		{[]string{"cp", source, "origin:/client/target"}, "download", "/client/target", false},
		{[]string{"forward", "origin:", "3000"}, "forward", "", false},
		{[]string{"forward", "3000", "origin:8080"}, "forward", "8080", true},
	} {
		_, done := e.helper(tc.args[0], tc.args[1:]...)
		stream, err := b.mux.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		m := recv(t, stream)
		if m.Type != "approval_request" || len(m.Operations) != 1 {
			t.Fatal(m)
		}
		op := m.Operations[0]
		if op.Kind != tc.kind || op.LocalHint != tc.hint || op.Reverse != tc.reverse {
			t.Fatal(op)
		}
		if op.Kind == "forward" && (!op.localPortPending() || op.serverPort() != 3000) {
			t.Fatal("resolved without local consent", op)
		}
		sendTest(t, stream, message{Type: "queued"})
		if m := recv(t, stream); m.Type != "queued_ack" {
			t.Fatal(m)
		}
		stream.Close()
		waitHelper(t, done, true)
	}
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
}

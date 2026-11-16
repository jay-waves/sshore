package main

import (
	"strings"
	"testing"
)

func TestPSTreeReattachmentAncestry(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("tree")
	shellOutput(t, b, "printf 'START-%s' ready", "START-ready")
	detachTestClient(t, b)
	for _, parent := range []string{"parent-a", "parent-b"} {
		b = approvedTestAttach(t, e, "tree", pid, shellTreeNode{Identity: "origin", Host: "workstation", Name: "origin", State: "attached"}, shellTreeNode{Parent: "origin", Identity: parent, Host: "upstream", Name: parent, State: "attached"})
		resumeTestOutput(t, b)
		output := shellOutput(t, b, shellQuote(e.bin)+" pstree; printf 'TREE-%s' done", "TREE-done")
		if !strings.Contains(output, parent) || !strings.Contains(output, "← current") || !strings.Contains(output, "workstation / origin") {
			t.Fatalf("missing ancestry/current: %q", output)
		}
		if parent == "parent-b" && strings.Contains(output, "parent-a") {
			t.Fatalf("stale parent after attach: %q", output)
		}
		detachTestClient(t, b)
	}
	b, _ = e.attach("tree")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
	b.close()
}

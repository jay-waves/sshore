package main

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/jay-waves/sshore/internal/pty"
)

func treeTestShell(tasks *daemonTasks, parent *shellTask) *shellTask {
	return &shellTask{tasks: tasks, parent: parent, done: make(chan struct{}), record: localShellRecord{UID: newTaskUID()}}
}

func treeTestEnter(s *shellTask, key string) (*treeClaim, *shellTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks.forest.enter(s, key)
}

func treeTestAttach(t *testing.T, s *shellTask, key string) *treeClaim {
	t.Helper()
	claim, _, err := treeTestEnter(s, key)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func treeTestReject(t *testing.T, s *shellTask, key, reason string) {
	t.Helper()
	if _, _, err := treeTestEnter(s, key); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("want %q, got %v", reason, err)
	}
}

func treeTestHold(t *testing.T, s *shellTask, key, response string) func() {
	t.Helper()
	client, server := net.Pipe()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer server.Close()
		_ = s.holdTree(server, message{TreeKey: key, SoftwareVersion: version})
	}()
	release := func() {
		_ = client.Close()
		<-finished
	}
	t.Cleanup(release)
	if reply, err := readMessage(client); err != nil || reply.Type != response {
		t.Fatalf("hold response %+v, %v, want %s", reply, err, response)
	}
	return release
}

func TestShellForestOnlyAdmitsDirectParent(t *testing.T) {
	var tasks daemonTasks
	f := &tasks.forest
	root := treeTestShell(&tasks, nil)
	left, right := treeTestShell(&tasks, root), treeTestShell(&tasks, root)
	treeTestReject(t, left, "", "from its parent")
	first := treeTestAttach(t, root, "")
	treeTestHold(t, left, first.key, "error")
	closeRootHold := treeTestHold(t, root, first.key, "tree_held")
	treeTestHold(t, root, first.key, "error")
	treeTestReject(t, left, first.key, "reservation expired or is in use")
	f.releaseInput(first)
	if !root.suspendedBy(first.key) || left.suspendedBy(first.key) {
		t.Fatal("reservation does not identify the suspended parent")
	}
	treeTestReject(t, root, "", "tree is in use")
	child := treeTestAttach(t, left, first.key)
	treeTestReject(t, root, "", "tree is in use")
	treeTestReject(t, right, first.key, "tree is in use")
	closeChildHold := treeTestHold(t, left, child.key, "tree_held")
	f.releaseInput(child)
	treeTestReject(t, root, child.key, "ancestor")
	treeTestReject(t, right, child.key, "from its parent")
	treeTestReject(t, root, first.key, "tree is in use")
	closeChildHold()
	resumed := treeTestAttach(t, root, first.key)
	f.resume(root, first.key)
	if resumed.key == first.key || root.suspendedBy(first.key) {
		t.Fatal("resume reused a private reservation")
	}
	closeRootHold()
	f.releaseInput(resumed)
	treeTestReject(t, root, first.key, "reservation expired or is in use")
	treeTestReject(t, left, "", "from its parent")
	f.releaseInput(treeTestAttach(t, root, ""))
}

func TestShellForestSerializesGraftsAndRollsBackAttach(t *testing.T) {
	var tasks daemonTasks
	f := &tasks.forest
	root := treeTestShell(&tasks, nil)
	first := treeTestAttach(t, root, "")
	treeTestHold(t, root, first.key, "tree_held")
	f.releaseInput(first)

	left, right := treeTestShell(&tasks, nil), treeTestShell(&tasks, nil)
	left.pinned, left.scopeLost = true, true
	right.pinned, right.scopeLost = true, true
	other := treeTestShell(&tasks, nil)
	independent := treeTestAttach(t, other, "")
	treeTestReject(t, other, first.key, "tree is in use")
	f.releaseInput(independent)
	other.remoteOwner = newTaskUID()
	treeTestReject(t, other, first.key, "from its remote parent")
	type result struct {
		claim *treeClaim
		graft *shellTask
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, s := range []*shellTask{left, right} {
		go func() {
			<-start
			claim, graft, err := treeTestEnter(s, first.key)
			results <- result{claim, graft, err}
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil {
		a, b = b, a
	}
	if a.err != nil || a.graft != root || b.err == nil || !strings.Contains(b.err.Error(), "tree is in use") {
		t.Fatalf("graft results %+v and %+v", a, b)
	}
	winner := a.claim.shell
	treeTestReject(t, winner, "", "from its parent")
	f.abort(a.claim, a.graft, nil)

	// A rejected setup on an existing detached shell must leave its old root
	// and the source's resume credential available.
	winner.terminal = new(pty.Terminal)
	request := message{Type: "attach", TreeKey: first.key, Term: "xterm", NoProfile: true}
	err := winner.attachManaged(new(attachment), request, func(message) error {
		t.Fatal("started the existing shell again")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "startup options require a new managed shell") {
		t.Fatalf("existing-shell startup option returned %v", err)
	}
	f.releaseInput(treeTestAttach(t, winner, ""))
	if !root.suspendedBy(first.key) {
		t.Fatal("rollback released the source reservation")
	}
}

func TestShellForestReleasesOnlyAfterParentTerminalExit(t *testing.T) {
	var tasks daemonTasks
	f := &tasks.forest
	parent := treeTestShell(&tasks, nil)
	pinned := treeTestShell(&tasks, parent)
	pinned.pinned = true
	grandchild := treeTestShell(&tasks, pinned)
	unretained := treeTestShell(&tasks, parent)
	parentClaim := treeTestAttach(t, parent, "")
	closeParentHold := treeTestHold(t, parent, parentClaim.key, "tree_held")
	f.releaseInput(parentClaim)
	childClaim := treeTestAttach(t, pinned, parentClaim.key)
	f.releaseInput(childClaim)
	treeTestReject(t, pinned, "", "from its parent")
	closeParentHold()
	treeTestReject(t, pinned, "", "from its parent")

	parentClaim = treeTestAttach(t, parent, "")
	treeTestHold(t, parent, parentClaim.key, "tree_held")
	f.releaseInput(parentClaim)
	parent.mu.Lock()
	parent.endTerminal()
	parent.mu.Unlock()
	if parent.expired() || parent.suspendedBy(parentClaim.key) {
		t.Fatal("terminal exit should revoke the reservation before final cleanup")
	}
	treeTestReject(t, pinned, parentClaim.key, "reservation expired or is in use")
	f.releaseInput(treeTestAttach(t, pinned, ""))
	treeTestReject(t, grandchild, "", "from its parent")
	if _, _, err := treeTestEnter(unretained, ""); !errors.Is(err, errShellExited) {
		t.Fatalf("unretained child was available before its parent watcher ran: %v", err)
	}
	<-unretained.done
}

func TestShellForestEndedChildLetsWaitingParentResume(t *testing.T) {
	var tasks daemonTasks
	f := &tasks.forest
	grandparent := treeTestShell(&tasks, nil)
	parent := treeTestShell(&tasks, grandparent)
	child := treeTestShell(&tasks, parent)
	child.pinned = true
	first := treeTestAttach(t, grandparent, "")
	treeTestHold(t, grandparent, first.key, "tree_held")
	f.releaseInput(first)
	second := treeTestAttach(t, parent, first.key)
	treeTestHold(t, parent, second.key, "tree_held")
	f.releaseInput(second)
	third := treeTestAttach(t, child, second.key)
	treeTestReject(t, grandparent, first.key, "tree is in use")

	parent.mu.Lock()
	parent.endTerminal()
	parent.mu.Unlock()
	resumed := treeTestAttach(t, grandparent, first.key)
	f.resume(grandparent, first.key)
	// Both terminals now belong to different trees. A stale parent lease and
	// the live pinned grandchild cannot block the surviving ancestor.
	f.releaseInput(resumed)
	f.releaseInput(third)
	f.releaseInput(treeTestAttach(t, grandparent, ""))
	f.releaseInput(treeTestAttach(t, child, ""))
}

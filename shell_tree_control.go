package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"sync"
	"time"
)

// The registry's tree lock covers parent edges, terminal exit and input
// reservations. Never acquire a shell or registry lock while holding it.
type shellForest struct {
	mu     sync.Mutex
	claims map[string]*treeClaim
}

type treeClaim struct {
	shell *shellTask
	key   string
	input bool
	held  bool
}

// Call with the forest lock held.
func (s *shellTask) liveParent() *shellTask {
	if s.parent != nil && !s.parent.treeEnded.Load() {
		return s.parent
	}
	return nil
}

func treeRoot(s *shellTask) *shellTask {
	for s.liveParent() != nil {
		s = s.parent
	}
	return s
}

func onShellPath(s, ancestor *shellTask) bool {
	for ; s != nil; s = s.liveParent() {
		if s == ancestor {
			return true
		}
	}
	return false
}

// The caller holds the target shell lock. A suspended parent supplies its
// private claim over the original authenticated transport.
func (f *shellForest) enter(s *shellTask, key string) (*treeClaim, *shellTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := s.liveParent()
	s.reapUnretainedLocked()
	if s.exited || s.killing || s.treeEnded.Load() {
		return nil, nil, errShellExited
	}
	var source *shellTask
	if key != "" {
		c := f.claims[key]
		if c == nil || !c.held || c.input {
			return nil, nil, fmt.Errorf("parent terminal reservation expired or is in use")
		}
		source = c.shell
	}
	if source != nil && source != s && onShellPath(source, s) {
		return nil, nil, fmt.Errorf("target is an ancestor of the current shell")
	}
	if source != s && parent != nil && parent != source {
		return nil, nil, fmt.Errorf("enter shell %s from its parent %s", taskShortCode(s.treeIdentity()), taskShortCode(parent.treeIdentity()))
	}
	graft := source != nil && source != s && parent == nil
	if graft && s.remoteOwner != "" {
		return nil, nil, fmt.Errorf("enter shell %s from its remote parent", taskShortCode(s.treeIdentity()))
	}
	root := treeRoot(s)
	var sourceRoot *shellTask
	if source != nil {
		sourceRoot = treeRoot(source)
	}
	for _, c := range f.claims {
		if !c.input && onShellPath(source, c.shell) {
			continue
		}
		if r := treeRoot(c.shell); r == root || r == sourceRoot {
			return nil, nil, fmt.Errorf("shell tree is in use at %s", taskShortCode(c.shell.treeIdentity()))
		}
	}
	c := &treeClaim{shell: s, key: rand.Text(), input: true}
	if f.claims == nil {
		f.claims = make(map[string]*treeClaim)
	}
	f.claims[c.key] = c
	if graft {
		// Reserve the edge during PTY setup so a competing graft cannot form a cycle.
		s.parent = source
		return c, source, nil
	}
	return c, nil, nil
}

func (f *shellForest) waiting(s *shellTask) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.claims {
		if c.shell == s && c.held || c.shell != s && onShellPath(c.shell, s) {
			return true
		}
	}
	return false
}

func (f *shellForest) resume(s *shellTask, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old := f.claims[key]; old != nil && old.shell == s {
		delete(f.claims, key)
	}
}

func (f *shellForest) end(s *shellTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.treeEnded.Swap(true) {
		return
	}
	for key, c := range f.claims {
		if c.shell == s {
			delete(f.claims, key)
		}
	}
}

// The caller holds the target shell lock throughout attachment setup.
func (f *shellForest) abort(c *treeClaim, graft, oldParent *shellTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if graft != nil {
		c.shell.parent = oldParent
	}
	delete(f.claims, c.key)
}

func (f *shellForest) releaseInput(c *treeClaim) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c.input = false
	if !c.held {
		delete(f.claims, c.key)
	}
}

func (s *shellTask) suspendedBy(key string) bool {
	f := &s.tasks.forest
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.claims[key]
	return c != nil && c.shell == s && c.held && !c.input
}

func (s *shellTask) holdTree(conn net.Conn, m message) error {
	f := &s.tasks.forest
	f.mu.Lock()
	c := f.claims[m.TreeKey]
	if c == nil || c.shell != s || !c.input || c.held || checkPeerVersion(m.SoftwareVersion, "client") != nil {
		f.mu.Unlock()
		return writeMessage(conn, message{Type: "error", Error: "current shell attachment changed"})
	}
	c.held = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		c.held = false
		if !c.input {
			delete(f.claims, c.key)
		}
		f.mu.Unlock()
	}()
	if err := writeMessage(conn, message{Type: "tree_held"}); err != nil {
		return err
	}
	// EOF releases this reservation, including on client or SSH loss.
	var b [1]byte
	_, _ = conn.Read(b[:])
	return nil
}

func holdClientTree(o options, key string, transports clientTransports) (*clientTransport, error) {
	o.command = "attach"
	ctx, cancel := context.WithCancel(context.Background())
	timeout := time.AfterFunc(requestTimeout, cancel)
	defer timeout.Stop()
	c, err := openClientTransportContext(ctx, &o, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	reply, err := exchangeRouteRequest(c, message{Type: "tree_hold", TreeKey: key})
	if err != nil || reply.Type != "tree_held" {
		cancel()
		c.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("reserve shell tree: %s", reply.Error)
	}
	lease := &clientTransport{treeKey: key, close: func() { c.Close(); cancel() }}
	transports[lease] = false
	return lease, nil
}

// Package tmux manages the ownership of terminals used by sh and new.
package tmux

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

var ErrEnded = errors.New("session is exiting")

// A treeNode participates in a forest.
type treeNode struct {
	id     string
	Label  string
	done   <-chan struct{}
	parent atomic.Pointer[treeNode]
	ended  atomic.Bool
}

func (n *treeNode) Parent() *treeNode { return n.parent.Load() }
func (n *treeNode) Ended() bool       { return n.ended.Load() }

// A forest serializes changes to parent edges and reservations. The caller
// serializes the lifetime of each node with its own task lock.
type forest struct {
	mu     sync.Mutex
	claims map[string]*reservation
}

type reservation struct {
	node        *treeNode
	key         string
	input, held bool
}

func (f *forest) setParent(node, parent *treeNode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	node.parent.Store(parent)
}

func liveParent(node *treeNode) *treeNode {
	if parent := node.Parent(); parent != nil && !parent.Ended() {
		return parent
	}
	return nil
}

func root(node *treeNode) *treeNode {
	for parent := liveParent(node); parent != nil; parent = liveParent(node) {
		node = parent
	}
	return node
}

func onPath(node, ancestor *treeNode) bool {
	for ; node != nil; node = liveParent(node) {
		if node == ancestor {
			return true
		}
	}
	return false
}

type attachmentReservation struct {
	claim                      *reservation
	sourceKey                  string
	parent, oldParent, adopted *treeNode
}

// finishAttach commits the reservation or gives the tree back to its previous owner.
func (f *forest) finishAttach(r *attachmentReservation, commit bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !commit || liveParent(r.claim.node) != r.parent {
		if node := r.claim.node; r.adopted != nil && node.Parent() == r.adopted {
			node.parent.Store(r.oldParent)
		}
		delete(f.claims, r.claim.key)
		return false
	}
	if old := f.claims[r.sourceKey]; r.adopted == nil && old != nil && old.node == r.claim.node {
		delete(f.claims, r.sourceKey)
	}
	return true
}

// canAttach runs under the forest lock and must not take another lock.
func (f *forest) reserve(node *treeNode, key string, canAttach func() bool) (*attachmentReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !canAttach() || node.Ended() {
		return nil, ErrEnded
	}
	parent := liveParent(node)
	var source *treeNode
	if key != "" {
		claim := f.claims[key]
		if claim == nil || !claim.held || claim.input {
			return nil, errors.New("parent terminal reservation expired or is in use")
		}
		source = claim.node
	}
	if source != nil && source != node && onPath(source, node) {
		return nil, errors.New("target is an ancestor of the current shell")
	}
	if source != node && parent != nil && parent != source {
		return nil, fmt.Errorf("enter shell %s from its parent %s", node.Label, parent.Label)
	}
	adopt := source != nil && source != node && parent == nil
	targetRoot := root(node)
	var sourceRoot *treeNode
	if source != nil {
		sourceRoot = root(source)
	}
	for _, claim := range f.claims {
		if !claim.input && onPath(source, claim.node) {
			continue
		}
		if r := root(claim.node); r == targetRoot || r == sourceRoot {
			return nil, fmt.Errorf("shell tree is in use at %s", claim.node.Label)
		}
	}
	claim := &reservation{node: node, key: rand.Text(), input: true}
	if f.claims == nil {
		f.claims = make(map[string]*reservation)
	}
	f.claims[claim.key] = claim
	r := &attachmentReservation{claim: claim, sourceKey: key, parent: parent, oldParent: node.Parent()}
	if adopt {
		node.parent.Store(source)
		r.parent, r.adopted = source, source
	}
	return r, nil
}

func (f *forest) waiting(node *treeNode) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, claim := range f.claims {
		if claim.node == node && claim.held || claim.node != node && onPath(claim.node, node) {
			return true
		}
	}
	return false
}

func (f *forest) end(node *treeNode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if node.ended.Swap(true) {
		return
	}
	for key, claim := range f.claims {
		if claim.node == node {
			delete(f.claims, key)
		}
	}
}

func (f *forest) releaseInput(claim *reservation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	claim.input = false
	if !claim.held {
		delete(f.claims, claim.key)
	}
}

// hold retains the reservation after the input connection closes. The returned
// function releases it when the authenticated hold connection closes.
func (f *forest) hold(node *treeNode, key string) (release func(), ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	claim := f.claims[key]
	if claim == nil || claim.node != node || !claim.input || claim.held {
		return nil, false
	}
	claim.held = true
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		claim.held = false
		if !claim.input {
			delete(f.claims, key)
		}
	}, true
}

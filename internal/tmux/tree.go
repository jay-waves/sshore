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

// treeNode participates in a forest. Its value and label are set before registration.
type treeNode[T any] struct {
	id     string
	Value  T
	Label  string
	done   <-chan struct{}
	parent atomic.Pointer[treeNode[T]]
	ended  atomic.Bool
}

func (n *treeNode[T]) Parent() *treeNode[T] { return n.parent.Load() }
func (n *treeNode[T]) Ended() bool          { return n.ended.Load() }

// Forest serializes changes to parent edges and reservations. The caller
// serializes the lifetime of each node with its own task lock.
type Forest[T any] struct {
	mu     sync.Mutex
	claims map[string]*reservation[T]
}

type reservation[T any] struct {
	node        *treeNode[T]
	key         string
	input, held bool
}

func (f *Forest[T]) setParent(node, parent *treeNode[T]) {
	f.mu.Lock()
	defer f.mu.Unlock()
	node.parent.Store(parent)
}

func liveParent[T any](node *treeNode[T]) *treeNode[T] {
	if parent := node.Parent(); parent != nil && !parent.Ended() {
		return parent
	}
	return nil
}

func root[T any](node *treeNode[T]) *treeNode[T] {
	for parent := liveParent(node); parent != nil; parent = liveParent(node) {
		node = parent
	}
	return node
}

func onPath[T any](node, ancestor *treeNode[T]) bool {
	for ; node != nil; node = liveParent(node) {
		if node == ancestor {
			return true
		}
	}
	return false
}

// attach reserves input before activation and restores the original tree if
// activation fails. Both callbacks run while the caller holds the task lock.
// canAttach runs under the forest lock and must not take another lock.
func (f *Forest[T]) attach(node *treeNode[T], key string, remoteParent bool, canAttach func() bool, activate func(*reservation[T]) error) (*treeNode[T], error) {
	oldParent := node.Parent()
	claim, adopted, err := f.reserve(node, key, remoteParent, canAttach)
	if err != nil {
		return nil, err
	}
	err = activate(claim)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		if adopted != nil {
			node.parent.Store(oldParent)
		}
		delete(f.claims, claim.key)
		return nil, err
	}
	if old := f.claims[key]; adopted == nil && old != nil && old.node == node {
		delete(f.claims, key)
	}
	return adopted, nil
}

func (f *Forest[T]) reserve(node *treeNode[T], key string, remoteParent bool, canAttach func() bool) (*reservation[T], *treeNode[T], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !canAttach() || node.Ended() {
		return nil, nil, ErrEnded
	}
	parent := liveParent(node)
	var source *treeNode[T]
	if key != "" {
		claim := f.claims[key]
		if claim == nil || !claim.held || claim.input {
			return nil, nil, errors.New("parent terminal reservation expired or is in use")
		}
		source = claim.node
	}
	if source != nil && source != node && onPath(source, node) {
		return nil, nil, errors.New("target is an ancestor of the current shell")
	}
	if source != node && parent != nil && parent != source {
		return nil, nil, fmt.Errorf("enter shell %s from its parent %s", node.Label, parent.Label)
	}
	adopt := source != nil && source != node && parent == nil
	if adopt && remoteParent {
		return nil, nil, fmt.Errorf("enter shell %s from its remote parent", node.Label)
	}
	targetRoot := root(node)
	var sourceRoot *treeNode[T]
	if source != nil {
		sourceRoot = root(source)
	}
	for _, claim := range f.claims {
		if !claim.input && onPath(source, claim.node) {
			continue
		}
		if r := root(claim.node); r == targetRoot || r == sourceRoot {
			return nil, nil, fmt.Errorf("shell tree is in use at %s", claim.node.Label)
		}
	}
	claim := &reservation[T]{node: node, key: rand.Text(), input: true}
	if f.claims == nil {
		f.claims = make(map[string]*reservation[T])
	}
	f.claims[claim.key] = claim
	if adopt {
		node.parent.Store(source)
		return claim, source, nil
	}
	return claim, nil, nil
}

func (f *Forest[T]) waiting(node *treeNode[T]) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, claim := range f.claims {
		if claim.node == node && claim.held || claim.node != node && onPath(claim.node, node) {
			return true
		}
	}
	return false
}

func (f *Forest[T]) end(node *treeNode[T]) {
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

func (f *Forest[T]) releaseInput(claim *reservation[T]) {
	f.mu.Lock()
	defer f.mu.Unlock()
	claim.input = false
	if !claim.held {
		delete(f.claims, claim.key)
	}
}

func (f *Forest[T]) suspended(node *treeNode[T], key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	claim := f.claims[key]
	return claim != nil && claim.node == node && claim.held && !claim.input
}

// hold retains the reservation after the input connection closes. The returned
// function releases it when the authenticated hold connection closes.
func (f *Forest[T]) hold(node *treeNode[T], key string) (release func(), ok bool) {
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

package tmux

import (
	"errors"
	"testing"
)

func TestFailedAdoptionLeavesParentReservationAvailable(t *testing.T) {
	var tree forest
	attach := func(node *treeNode, key string, canAttach func() bool, activate func(*reservation) error) (*treeNode, error) {
		r, err := tree.reserve(node, key, canAttach)
		if err != nil {
			return nil, err
		}
		err = activate(r.claim)
		if !tree.finishAttach(r, err == nil) && err == nil {
			err = ErrUnavailable
		}
		return r.adopted, err
	}
	parent, target := &treeNode{Label: "parent"}, &treeNode{Label: "target"}
	allow := func() bool { return true }
	var parentClaim *reservation
	if _, err := attach(parent, "", allow, func(claim *reservation) error {
		parentClaim = claim
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	release, ok := tree.hold(parent, parentClaim.key)
	if !ok {
		t.Fatal("could not reserve the parent's terminal")
	}
	defer release()
	tree.releaseInput(parentClaim)

	failure := errors.New("terminal setup failed")
	if _, err := attach(target, parentClaim.key, allow, func(*reservation) error {
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("failed attachment returned %v", err)
	}
	if target.Parent() != nil || !tree.waiting(parent) {
		t.Fatal("failed attachment changed the tree or consumed the parent's reservation")
	}
	var childClaim *reservation
	adopted, err := attach(target, parentClaim.key, allow, func(claim *reservation) error {
		childClaim = claim
		return nil
	})
	if err != nil || adopted != parent || target.Parent() != parent || !tree.waiting(parent) {
		t.Fatalf("adoption did not suspend the parent: %v", err)
	}
	if _, err := attach(parent, parentClaim.key, allow, func(*reservation) error {
		t.Fatal("parent received input during the child's attachment")
		return nil
	}); err == nil {
		t.Fatal("parent's reservation was accepted while its child owned input")
	}
	tree.releaseInput(childClaim)
	if _, err := attach(parent, parentClaim.key, allow, func(*reservation) error { return nil }); err != nil {
		t.Fatalf("parent could not resume after child detach: %v", err)
	}
	if tree.waiting(parent) {
		t.Fatal("resume retained the old reservation")
	}
}

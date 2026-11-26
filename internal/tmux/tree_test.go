package tmux

import (
	"errors"
	"testing"
)

func TestFailedAdoptionLeavesParentReservationAvailable(t *testing.T) {
	var tree Forest[string]
	parent, target := &treeNode[string]{Label: "parent"}, &treeNode[string]{Label: "target"}
	allow := func() bool { return true }
	var parentClaim *reservation[string]
	if _, err := tree.attach(parent, "", false, allow, func(claim *reservation[string]) error {
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
	if _, err := tree.attach(target, parentClaim.key, false, allow, func(*reservation[string]) error {
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("failed attachment returned %v", err)
	}
	if target.Parent() != nil || !tree.suspended(parent, parentClaim.key) {
		t.Fatal("failed attachment changed the tree or consumed the parent's reservation")
	}
	var childClaim *reservation[string]
	adopted, err := tree.attach(target, parentClaim.key, false, allow, func(claim *reservation[string]) error {
		childClaim = claim
		return nil
	})
	if err != nil || adopted != parent || target.Parent() != parent || !tree.waiting(parent) {
		t.Fatalf("adoption did not suspend the parent: %v", err)
	}
	if _, err := tree.attach(parent, parentClaim.key, false, allow, func(*reservation[string]) error {
		t.Fatal("parent received input during the child's attachment")
		return nil
	}); err == nil {
		t.Fatal("parent's reservation was accepted while its child owned input")
	}
	tree.releaseInput(childClaim)
	if _, err := tree.attach(parent, parentClaim.key, false, allow, func(*reservation[string]) error { return nil }); err != nil {
		t.Fatalf("parent could not resume after child detach: %v", err)
	}
	if tree.waiting(parent) || tree.suspended(parent, parentClaim.key) {
		t.Fatal("resume retained the old reservation")
	}
}

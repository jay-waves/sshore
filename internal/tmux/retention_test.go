package tmux

import (
	"testing"
	"time"
)

func TestExpiredPinKeepsActiveShellUntilDetach(t *testing.T) {
	var manager Manager
	parent, err := manager.Create(SessionConfig{ID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Create(SessionConfig{ID: "child", Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	// Exercise retention with the parent marked as ended, independently of its watcher.
	parent.node.ended.Store(true)
	defer parent.Close()
	defer child.Close()
	deadline := time.Now()
	child.pinUntil = &deadline
	child.active = new(Attachment)

	if child.unretained(deadline) {
		t.Fatal("parent exit and pin expiry stopped an active shell")
	}
	child.active = nil
	if !child.unretained(deadline) {
		t.Fatal("the expired pin kept the detached shell alive")
	}
}

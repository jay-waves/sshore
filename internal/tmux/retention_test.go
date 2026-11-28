package tmux

import (
	"sync"
	"testing"
	"time"
)

func TestExpiredPinKeepsActiveShellUntilDetach(t *testing.T) {
	var manager Manager[*Attachment, string]
	parent := manager.NewSession("parent", "parent", "parent", nil, new(sync.Mutex), Events[*Attachment, string]{})
	child := manager.NewSession("child", "child", "child", parent, new(sync.Mutex), Events[*Attachment, string]{})
	deadline := time.Now()
	child.pinUntil = &deadline
	child.active = new(Attachment)
	parent.Close()

	if child.unretained(deadline) {
		t.Fatal("parent exit and pin expiry stopped an active shell")
	}
	child.active = nil
	if !child.unretained(deadline) {
		t.Fatal("the expired pin kept the detached shell alive")
	}
}

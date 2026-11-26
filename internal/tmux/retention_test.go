package tmux

import (
	"testing"
	"time"
)

func TestExpiredPinKeepsActiveShellUntilDetach(t *testing.T) {
	var forest Forest[string]
	parent := NewTask[*Attachment](&forest, nil)
	child := NewTask[*Attachment](&forest, &parent)
	deadline := time.Now()
	child.pinUntil = &deadline
	child.active = new(Attachment)
	parent.End()

	if child.Unretained(deadline) {
		t.Fatal("parent exit and pin expiry stopped an active shell")
	}
	child.active = nil
	if !child.Unretained(deadline) {
		t.Fatal("the expired pin kept the detached shell alive")
	}
}

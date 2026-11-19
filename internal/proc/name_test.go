package proc

import (
	"os"
	"testing"
)

func TestVerifiedName(t *testing.T) {
	pid := os.Getpid()
	start, err := LiveIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	if name := VerifiedName(pid, start); name == "" {
		t.Fatal("current process has no verified name")
	}
	if name := VerifiedName(pid, start+"-stale"); name != "" {
		t.Fatalf("stale identity returned process name %q", name)
	}
}

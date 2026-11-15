package main

import (
	"runtime"
	"slices"
	"testing"
)

func TestLocalShellEnvironment(t *testing.T) {
	source := []string{"SSHORE_SOCKET=remote", "SSH_CONNECTION=old", "SSHORE_LOCAL_SESSION=old", "TERM=old", "sshore_socket=mixed", "Term=mixed", "KEEP=value"}
	got := localShellEnvironment(source, "xterm", "local:new")
	for _, old := range source[:4] {
		if slices.Contains(got, old) {
			t.Fatalf("retained %s", old)
		}
	}
	for _, mixed := range source[4:6] {
		if slices.Contains(got, mixed) == (runtime.GOOS == "windows") {
			t.Fatalf("incorrect case handling: %s", mixed)
		}
	}
	for _, want := range []string{"KEEP=value", "TERM=xterm", "SSHORE_LOCAL_SESSION=local:new"} {
		if !slices.Contains(got, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if source[0] != "SSHORE_SOCKET=remote" {
		t.Fatal("mutated source environment")
	}
}

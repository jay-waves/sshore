package main

import (
	"strings"
	"testing"
)

func TestForwardDiagnosticsBounded(t *testing.T) {
	var d forwardDiagnostics
	d.Write([]byte(strings.Repeat("x", 8192)))
	d.Write([]byte("\nError: remote port forwarding failed for listen port 5173\n"))
	if len(d.data) > 4096 || !strings.Contains(d.String(), "listen port 5173") {
		t.Fatalf("diagnostic tail lost: %q", d.String())
	}
}

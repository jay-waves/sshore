package main

import (
	"os"
	"testing"
)

func TestRedirectedLogsArePlain(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	logf(f, logError, "sshore: %s\n", "failed")
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "sshore: failed\n" {
		t.Fatalf("%q", b)
	}
}

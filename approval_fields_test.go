package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApprovalPathRetry(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists")
	if err := os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	validate := func(path string) (string, error) { return downloadDestination(path, "new") }
	input := &stubSetupInput{answers: []string{existing, filepath.Join(dir, "missing", "new"), dir}}
	path, err := readApprovalPath(input, "Save as: ", "", validate)
	if err != nil || path != filepath.Join(dir, "new") {
		t.Fatal(path, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("validation created a file", err)
	}
	if data, _ := os.ReadFile(existing); string(data) != "keep" {
		t.Fatal("validation overwrote a file")
	}
	// A default is preserved literally, including spaces. Only entered input is parsed.
	defaultPath := filepath.Join(dir, "new with trailing space ")
	path, err = readApprovalPath(&stubSetupInput{answers: []string{""}}, "Save as: ", defaultPath, validate)
	if err != nil || path != defaultPath {
		t.Fatal(path, err)
	}
}

func TestApprovalPathCancellation(t *testing.T) {
	calls := 0
	_, err := readApprovalPath(&stubSetupInput{answers: []string{""}}, "File: ", "", func(path string) (string, error) { calls++; return path, nil })
	if err == nil || !strings.Contains(err.Error(), "remains pending") || !strings.Contains(err.Error(), io.EOF.Error()) || calls != 0 {
		t.Fatal(err, calls)
	}
}

func TestDisplayBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1 << 20: "1.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := displayBytes(n); got != want {
			t.Fatal(n, got, want)
		}
	}
}

func TestSelectedPort(t *testing.T) {
	for _, tc := range []struct {
		answers []string
		hint    string
		want    int
	}{
		{[]string{"", "0", "65536", "8080"}, "", 8080},
		{[]string{""}, "3000", 3000},
		{[]string{"8080"}, "3000", 8080},
	} {
		port, err := readSelectedPort(&stubSetupInput{answers: tc.answers}, "Local port", tc.hint, "cancelled")
		if err != nil || port != tc.want {
			t.Fatal(port, err)
		}
	}
	if _, err := readSelectedPort(&stubSetupInput{}, "Local port", "3000", "cancelled"); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatal("EOF accepted a suggestion", err)
	}
}

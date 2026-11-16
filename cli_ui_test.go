package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jay-waves/sshore/internal/files"
)

func TestSessionAge(t *testing.T) {
	if sessionAge(nil) != "" {
		t.Fatal("missing age displayed")
	}
	for _, tc := range []struct {
		seconds int64
		want    string
	}{
		{-1, "0s"}, {0, "0s"}, {59, "59s"}, {60, "1m"}, {3599, "59m"},
		{3600, "1h"}, {86399, "23h"}, {86400, "1d"}, {172800, "2d"},
	} {
		if got := sessionAge(&tc.seconds); got != " · started "+tc.want+" ago" {
			t.Fatalf("%d: %q", tc.seconds, got)
		}
	}
}

func TestShortDirectory(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"", "—"}, {"/", "/"}, {"/tmp", "/tmp"}, {"/home/me", "~"},
		{"/home/me/project", "~/project"}, {"/home/me/src/project", "~/src/project"},
		{"/home/me/work/src/project", "…/src/project"}, {"/var/lib/project", "…/lib/project"},
		{"/home/mean/project", "…/mean/project"}, {"/tmp/evil\x1b", "/tmp/evil�"},
	} {
		if got := shortDirectory(tc.path, "/home/me"); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.path, got, tc.want)
		}
	}
	if got := shortDirectory("/"+strings.Repeat("中", 100), ""); len([]rune(got)) != 40 {
		t.Fatal(got)
	}
}

func TestRequestPickerIdentityAndFilter(t *testing.T) {
	ids := []string{"first-id", "second-id"}
	labels := []string{"first-id · alpha · upload /tmp", "second-id · beta · download /tmp"}
	for _, tc := range []struct {
		answers []string
		want    string
	}{
		{[]string{"/beta", "#1", "#2"}, "second-id"},
		{[]string{"/beta", "/", "first-id"}, "first-id"},
		{[]string{""}, ""},
	} {
		got, err := chooseLocalItem(&stubSetupInput{answers: tc.answers}, ids, labels, "Request: ", "request")
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatal(got, err)
		}
	}
}

func TestApprovalPathRetry(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists")
	if err := os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	validate := func(path string) (string, error) { return files.DownloadDestination(path, "new") }
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
	// Defaults bypass input parsing, but still use native absolute-path normalization.
	defaultPath := filepath.Join(dir, " new with surrounding spaces ")
	want, err := filepath.Abs(defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	path, err = readApprovalPath(&stubSetupInput{answers: []string{""}}, "Save as: ", defaultPath, validate)
	if err != nil || path != want {
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

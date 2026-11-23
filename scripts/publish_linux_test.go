package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishRelease(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := `#!/bin/sh
test "$GH_TOKEN" = test || exit 25
printf '%s\n' "$*" > "$PUBLISH_TEST_CALLS"
cat >> "$PUBLISH_TEST_CALLS"
exit "$PUBLISH_TEST_STATUS"
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PUBLISH_TEST_CALLS", calls)
	t.Setenv("GITHUB_SHA", "abc")
	build := func(version string) ([]string, error) {
		if version != "0.4.1" || os.Getenv("GH_TOKEN") != "" {
			t.Fatalf("unexpected build environment for %s", version)
		}
		return []string{"packages/linux.tar.gz", "packages/windows.zip", "packages/SHA256SUMS"}, nil
	}
	for _, tag := range []string{"v0.4", "v0.0.0"} {
		t.Setenv("GITHUB_REF", "refs/tags/"+tag)
		if err := publish(build); err == nil {
			t.Fatalf("accepted tag %s", tag)
		}
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("publisher was invoked for an invalid tag: %v", err)
	}
	t.Setenv("GITHUB_REF", "refs/tags/v0.4.1")
	for _, status := range []string{"0", "1"} {
		t.Setenv("GH_TOKEN", "test")
		t.Setenv("PUBLISH_TEST_STATUS", status)
		if err := publish(build); (err != nil) != (status == "1") {
			t.Fatalf("gh status %s: %v", status, err)
		}
		data, err := os.ReadFile(calls)
		if err != nil {
			t.Fatal(err)
		}
		args, notes, _ := strings.Cut(string(data), "\n")
		if !strings.HasPrefix(args, "release create v0.4.1 --verify-tag ") ||
			!strings.HasSuffix(args, "packages/linux.tar.gz packages/windows.zip packages/SHA256SUMS") ||
			!strings.Contains(notes, "Commit: abc\n") {
			t.Fatalf("unexpected publication request: %s", data)
		}
	}
}

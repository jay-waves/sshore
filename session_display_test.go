package main

import (
	"strings"
	"testing"
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

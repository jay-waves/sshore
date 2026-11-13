package main

import "testing"

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

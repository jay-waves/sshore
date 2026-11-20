package main

import (
	"slices"
	"testing"
)

func TestConsoleCompletion(t *testing.T) {
	complete := consoleCompleter(func() []sessionTarget {
		return []sessionTarget{{Name: "alpha"}}
	})
	for _, tc := range []struct {
		line string
		want []string
	}{
		{"p", []string{"s ", "stree "}},
		{"attach al", []string{"pha "}},
		{"a al", []string{"pha "}},
		{"ls", []string{" "}},
		{"sh", []string{" "}},
		{"mux", []string{" "}},
		{"cp", nil},
		{"forward", nil},
	} {
		line := []rune(tc.line)
		suffixes, _ := complete.Do(line, len(line))
		var got []string
		for _, suffix := range suffixes {
			got = append(got, string(suffix))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("completing %q: got %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestConsoleInventoryBody(t *testing.T) {
	if got := consoleInventoryBody("first\nsecond\nthird", 10); got != "first\nsecond\n"+consoleMoreRows {
		t.Fatalf("inventory body: %q", got)
	}
}

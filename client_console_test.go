package main

import (
	"slices"
	"testing"

	"github.com/jay-waves/sshore/internal/termui"
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
		{"sh", []string{" ", "ell "}},
		{"de", []string{"tach "}},
		{"d", []string{" ", "etach "}},
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

func TestConsoleConfirmationRequiresExplicitYes(t *testing.T) {
	for _, answer := range []string{"", "n", "detach", "yes", "cancel"} {
		if termui.ConfirmYes(answer) {
			t.Errorf("accepted %q", answer)
		}
	}
	for _, answer := range []string{"y", "Y"} {
		if !termui.ConfirmYes(answer) {
			t.Errorf("rejected %q", answer)
		}
	}
}

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestPingSummaryStylesPreserveMeasurements(t *testing.T) {
	summary := pingSummary{connection: 1234 * time.Microsecond, batches: []pingBatch{{minimum: 7 * time.Microsecond, total: 24 * time.Microsecond, maximum: 9 * time.Microsecond, count: 3}}}
	want := "SSH connection: 1.234ms\nEcho RTT 1 (3 samples): min 7µs, avg 8µs, max 9µs"
	plain, styled := summary.render(false), summary.render(true)
	if plain != want {
		t.Fatalf("plain summary: %q", plain)
	}
	if !strings.Contains(styled, "\x1b[") || ansi.Strip(styled) != want {
		t.Fatalf("styled summary: %q", styled)
	}
}

func TestPingSummaryShowsEachGroup(t *testing.T) {
	group := pingBatch{minimum: time.Millisecond, total: 20 * time.Millisecond, maximum: 3 * time.Millisecond, count: 10}
	summary := pingSummary{connection: time.Millisecond, batches: []pingBatch{group, group, group}}
	text := summary.render(false)
	for _, want := range []string{"SSH connection: 1ms", "Echo RTT 1 (10 samples)", "Echo RTT 2 (10 samples)", "Echo RTT 3 (10 samples)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Count(text, "\n") != 3 {
		t.Fatalf("wrong row count: %q", text)
	}
}

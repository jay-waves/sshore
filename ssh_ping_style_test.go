package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestPingSummaryStylesPreserveMeasurements(t *testing.T) {
	summary := pingSummary{connection: 1234 * time.Microsecond, minimum: 7 * time.Microsecond, average: 8 * time.Microsecond, maximum: 9 * time.Microsecond, count: 3}
	want := "SSH connection: 1.234ms\nEcho RTT (3 samples): min 7µs, avg 8µs, max 9µs"
	plain, styled := summary.render(false), summary.render(true)
	if plain != want {
		t.Fatalf("plain summary: %q", plain)
	}
	if !strings.Contains(styled, "\x1b[") || ansi.Strip(styled) != want {
		t.Fatalf("styled summary: %q", styled)
	}
}

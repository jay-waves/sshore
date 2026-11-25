package termui

import (
	"strings"
	"testing"
)

func TestConsoleStyleFiltering(t *testing.T) {
	const input = "\x1b[1;36mheader\x1b[m \x1b[2J \x1b]0;title\a"
	plain := cleanConsoleLine(input, false)
	if strings.ContainsRune(plain, '\x1b') || !strings.Contains(plain, "header") {
		t.Fatalf("plain: %q", plain)
	}
	styled := cleanConsoleLine(input, true)
	if !strings.Contains(styled, "\x1b[1;36mheader\x1b[m") || strings.Contains(styled, "\x1b[2J") || strings.Contains(styled, "\x1b]") {
		t.Fatalf("styled: %q", styled)
	}
	if !strings.HasSuffix(styled, "\x1b[m") {
		t.Fatalf("styled line must end with a reset: %q", styled)
	}
	if got := cleanConsoleLine("\x1b[1mheading", true); got != "\x1b[1mheading\x1b[m" {
		t.Fatalf("unclosed style: %q", got)
	}
}

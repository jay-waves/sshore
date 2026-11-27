package termui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestTableWidth(t *testing.T) {
	rows := [][]TableCell{
		{{Text: "ID"}, {Text: "STATE"}, {Text: "DETAIL"}},
		{{Text: "会話"}, {Text: "R", Color: Success}, {Text: strings.Repeat("詳細", 30)}},
	}
	var out strings.Builder
	WriteTable(&out, rows, 64, -1, true)
	wide := ansi.Strip(out.String())
	if !strings.HasPrefix(wide, "╭") || !strings.Contains(wide, "…") {
		t.Fatalf("expected a bordered table with truncated details:\n%s", wide)
	}
	for _, row := range strings.Split(strings.TrimSuffix(wide, "\n"), "\n") {
		if width := ansi.StringWidth(row); width > 64 {
			t.Fatalf("row exceeds terminal width (%d): %q", width, row)
		}
	}
	out.Reset()
	WriteTable(&out, rows, 12, -1, false)
	if narrow := out.String(); strings.Contains(narrow, "DETAIL") || !strings.Contains(narrow, "会話") {
		t.Fatalf("narrow table should retain the fixed columns:\n%s", narrow)
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/chzyer/readline"
)

func TestCompletionListing(t *testing.T) {
	candidates := [][]rune{[]rune("folder/"), []rune("file.txt")}
	text, fits := completionListing(candidates, "", 80, 24, true)
	if !fits || !strings.Contains(text, "\x1b[36mfolder/\x1b[39m") || strings.Contains(text, "\x1b[36mfile.txt") {
		t.Fatalf("directory-only color: %q", text)
	}
	text, _ = completionListing(candidates, "", 80, 24, false)
	if strings.Contains(text, "\x1b") {
		t.Fatal("uncolored output contains escapes", text)
	}
	for _, size := range [][2]int{{80, 6}, {10, 24}} {
		text, fits = completionListing(candidates, "long-prefix-", size[0], size[1], true)
		if fits || !strings.Contains(text, "2 matches;") || strings.Contains(text, "folder") {
			t.Fatalf("small terminal should show hint: %q", text)
		}
	}
	text, fits = completionListing(make([][]rune, 200), "", 80, 24, true)
	if fits || text != "200 matches; type more to narrow the list.\n" {
		t.Fatal(text)
	}
}

func TestCompletionSelectionPreservesPath(t *testing.T) {
	p := &approvalInput{paths: true, completion: pathCompletion{
		line: []rune(`"folder/文"`), pos: len([]rune(`"folder/文`)),
		candidates: [][]rune{[]rune("件 a.txt"), []rune("件 b.txt")}, selected: -1,
	}}
	for _, want := range []string{`"folder/文件 a.txt"`, `"folder/文件 b.txt"`, `"folder/文件 a.txt"`} {
		p.Do(nil, 0)
		line, pos, ok := p.completionChanged(nil, 0, readline.CharTab)
		if !ok || string(line) != want || pos != len([]rune(want))-1 {
			t.Fatalf("selection: %q at %d, want %q", string(line), pos, want)
		}
	}
	if _, process := p.filterCompletionKey(readline.CharEnter); process {
		t.Fatal("selection submitted upload")
	}
	if key, _ := p.filterCompletionKey(readline.CharEnter); key != readline.CharEnter {
		t.Fatal("second Enter did not submit")
	}
}

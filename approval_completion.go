package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/chzyer/readline"
	"golang.org/x/term"
)

// Keep readline's editing, but not its unbounded, repeatedly redrawn menu.
// Candidate strings remain plain paths; decoration is display-only.
type pathCompletion struct {
	line       []rune
	pos        int
	candidates [][]rune
	selected   int
	pending    []rune
	pendingPos int
	notice     string
}

func (p *approvalInput) filterCompletionKey(key rune) (rune, bool) {
	if key == readline.CharTab {
		return key, true
	}
	selected := len(p.completion.candidates) > 0 && p.completion.selected >= 0
	p.completion = pathCompletion{}
	if selected && (key == readline.CharEnter || key == readline.CharCtrlJ) {
		// Accept the candidate without submitting an upload on the same Enter.
		// Filtering also tells readline to resume reading after the newline.
		return key, false
	}
	return key, true
}

func (p *approvalInput) completionChanged(line []rune, pos int, key rune) ([]rune, int, bool) {
	if p.completion.pending == nil {
		return nil, 0, false
	}
	line, pos = p.completion.pending, p.completion.pendingPos
	p.completion.pending = nil
	return line, pos, true
}

func (p *approvalInput) completePath(line []rune, pos int) ([][]rune, int) {
	c := &p.completion
	if len(c.candidates) > 0 {
		c.selected = (c.selected + 1) % len(c.candidates)
		suffix := c.candidates[c.selected]
		c.pending = append([]rune(nil), c.line[:c.pos]...)
		c.pending = append(c.pending, suffix...)
		c.pending = append(c.pending, c.line[c.pos:]...)
		c.pendingPos = c.pos + len(suffix)
		return nil, 0
	}
	candidates, offset := completeLocalPath(line, pos)
	if len(candidates) <= 1 {
		return candidates, offset
	}
	if common, n := (readline.Runes{}).Aggregate(candidates); n > 0 {
		return [][]rune{common}, offset
	}
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		width, height = 80, 24
	}
	text, fits := completionListing(candidates, string(line[pos-offset:pos]), width, height, p.color)
	// Repeated Tab on a large directory prints the hint just once. Typing or
	// moving the cursor resets it through the key filter.
	if text != c.notice && p.editor != nil {
		fmt.Fprint(p.editor.Stdout(), text)
		c.notice = text
	}
	if fits {
		c.line, c.pos = append([]rune(nil), line...), pos
		c.candidates, c.selected = candidates, -1
	}
	return nil, 0
}

func completionListing(candidates [][]rune, base string, width, height int, color bool) (string, bool) {
	// One candidate per row, at most half a screen and never more than eight.
	// Count wrapped names too, rather than truncating potentially distinct paths.
	width = max(1, width-1)
	budget := min(8, height/2) - 2
	rows := 0
	for _, suffix := range candidates {
		cells := 2 + (readline.Runes{}).WidthAll([]rune(base+string(suffix)))
		rows += max(1, (cells+width-1)/width)
	}
	const help = "Tab: cycle | Enter: select | type to narrow\n"
	rows += (len(help) - 1 + width - 1) / width
	if rows > budget {
		return fmt.Sprintf("%d matches; type more to narrow the list.\n", len(candidates)), false
	}
	var b strings.Builder
	for _, suffix := range candidates {
		name := base + string(suffix)
		isDir := strings.HasSuffix(name, "/") || os.PathSeparator == '\\' && strings.HasSuffix(name, `\`)
		b.WriteString("  ")
		b.WriteString(formatLog(color && isDir, logInfo, "%s", name))
		b.WriteByte('\n')
	}
	b.WriteString(help)
	return b.String(), true
}

package termui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/chzyer/readline"
	"github.com/jay-waves/sshore/internal/sys"
	"golang.org/x/term"
)

// One readline editor owns stdin during setup or local approval and closes before
// SSH takes over. Input history is disabled. Paths are treated as a single string,
// including spaces, Unicode, optional surrounding quotes and home expansion;
// there is no shell command substitution or wildcard expansion. Completion occurs
// at the end or before a closing quote to preserve the suffix. Bounded menu display
// and selection are in completion.go; color never enters candidate paths.
// Windows hooks use checked console APIs and native VT when available, restoring
// console state on close. Legacy ANSI translation receives only uncolored text.

// Input owns one history-free line editor until Close returns stdin to its caller.
type Input struct {
	// Readline can deliver its final listener callback after Readline returns.
	// Serialize completion state with the next field's reset.
	completionMu  sync.Mutex
	editor        *readline.Instance
	paths         bool
	choices       []string
	color         bool
	restoreOutput func() error
	completion    pathCompletion
}

// NewInput creates an editor; the caller coordinates stdin ownership.
func NewInput() (*Input, error) {
	return NewInputFrom(nil)
}

// NewInputFrom creates an editor whose input is supplied by the caller.
func NewInputFrom(stdin io.ReadCloser) (*Input, error) {
	p := &Input{}
	config := &readline.Config{
		Stdin: stdin, AutoComplete: p, HistoryLimit: -1, DisableAutoSaveHistory: true,
		InterruptPrompt: "^C", EOFPrompt: "",
	}
	config.FuncFilterInputRune = p.filterCompletionKey
	config.Listener = readline.FuncListener(p.completionChanged)
	restore, ansi := configureInputTerminal(config)
	p.restoreOutput = restore
	p.color = ansi && ColorEnabled(os.Stdout)
	editor, err := readline.NewEx(config)
	if err != nil {
		_ = restore()
		return nil, err
	}
	p.editor = editor
	return p, nil
}

func (p *Input) Read(prompt string, paths bool) (string, error) {
	line, err := p.ReadPrefilled(prompt, paths, "")
	return strings.TrimSpace(line), err
}

func (p *Input) ReadPrefilled(prompt string, paths bool, initial string) (string, error) {
	return p.read(prompt, paths, nil, initial)
}

func (p *Input) ReadChoice(prompt string, choices []string) (string, error) {
	return p.read(prompt, false, choices, "")
}

func (p *Input) read(prompt string, paths bool, choices []string, initial string) (string, error) {
	p.completionMu.Lock()
	p.paths = paths
	p.choices = choices
	p.completion = pathCompletion{}
	p.completionMu.Unlock()
	p.editor.SetPrompt(FormatPrompt(p.color, prompt))
	// readline ignores errors from EnterRawMode. Check our Windows hook before
	// starting its reader; repeated entry is idempotent.
	if runtime.GOOS == "windows" {
		if err := p.editor.Config.FuncMakeRaw(); err != nil {
			return "", fmt.Errorf("prepare confirmation input: %w", err)
		}
	}
	return p.editor.ReadlineWithDefault(initial)
}

// ConfirmYes accepts an explicit affirmative answer to a default-no prompt.
func ConfirmYes(line string) bool {
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

func (p *Input) Close() error {
	if p.editor == nil {
		return nil
	}
	err := p.editor.Close()
	p.completionMu.Lock()
	p.editor = nil
	p.completionMu.Unlock()
	if p.restoreOutput != nil {
		err = errors.Join(err, p.restoreOutput())
		p.restoreOutput = nil
	}
	return err
}

func (p *Input) Do(line []rune, pos int) ([][]rune, int) {
	p.completionMu.Lock()
	defer p.completionMu.Unlock()
	if len(p.choices) > 0 && pos == len(line) {
		prefix := string(line)
		var candidates [][]rune
		for _, choice := range p.choices {
			if strings.HasPrefix(choice, prefix) {
				candidates = append(candidates, []rune(choice[len(prefix):]))
			}
		}
		return candidates, pos
	}
	if !p.paths {
		return nil, 0
	}
	return p.completePath(line, pos)
}

// CompleteLocalPath treats the input as one path, including spaces.
// Completion is offered at the end (or immediately before a closing quote),
// so inserting a candidate cannot corrupt a filename suffix after the cursor.
func CompleteLocalPath(line []rune, pos int) ([][]rune, int) {
	if pos < 0 || pos > len(line) {
		return nil, 0
	}
	prefix := string(line[:pos])
	quote := rune(0)
	if len(prefix) > 0 && (prefix[0] == '\'' || prefix[0] == '"') {
		quote = rune(prefix[0])
		prefix = prefix[1:]
	}
	if pos != len(line) && !(quote != 0 && pos == len(line)-1 && line[pos] == quote) {
		return nil, 0
	}
	dir, base := filepath.Split(prefix)
	searchDir := dir
	if searchDir == "" {
		searchDir = "."
	} else {
		var err error
		searchDir, err = sys.ExpandHome(searchDir)
		if err != nil {
			return nil, 0
		}
	}
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil, 0
	}
	sep := string(os.PathSeparator)
	if runtime.GOOS == "windows" && strings.Contains(dir, "/") && !strings.Contains(dir, `\`) {
		sep = "/"
	}
	var candidates [][]rune
	for _, entry := range entries {
		name := entry.Name()
		if strings.IndexFunc(name, unicode.IsControl) >= 0 || !strings.HasPrefix(name, base) {
			continue
		}
		isDir := entry.IsDir()
		if entry.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(searchDir, name)); err == nil {
				isDir = info.IsDir()
			}
		}
		suffix := strings.TrimPrefix(name, base)
		if isDir {
			suffix += sep
		}
		candidates = append(candidates, []rune(suffix))
	}
	return candidates, len([]rune(base))
}

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

// cycle inserts the next candidate while retaining text after the cursor.
func (c *pathCompletion) cycle() {
	c.selected = (c.selected + 1) % len(c.candidates)
	c.pending = append([]rune(nil), c.line[:c.pos]...)
	c.pending = append(c.pending, c.candidates[c.selected]...)
	c.pending = append(c.pending, c.line[c.pos:]...)
	c.pendingPos = c.pos + len(c.candidates[c.selected])
}

func (p *Input) filterCompletionKey(key rune) (rune, bool) {
	p.completionMu.Lock()
	defer p.completionMu.Unlock()
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

func (p *Input) completionChanged(line []rune, pos int, key rune) ([]rune, int, bool) {
	p.completionMu.Lock()
	defer p.completionMu.Unlock()
	if p.completion.pending == nil {
		return nil, 0, false
	}
	line, pos = p.completion.pending, p.completion.pendingPos
	p.completion.pending = nil
	return line, pos, true
}

func (p *Input) completePath(line []rune, pos int) ([][]rune, int) {
	c := &p.completion
	if len(c.candidates) > 0 {
		c.cycle()
		return nil, 0
	}
	candidates, offset := CompleteLocalPath(line, pos)
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
		b.WriteString(Format(color && isDir, Info, "%s", name))
		b.WriteByte('\n')
	}
	b.WriteString(help)
	return b.String(), true
}

func ParsePath(b []byte) (string, error) {
	if !utf8.Valid(b) {
		return "", fmt.Errorf("path must be UTF-8")
	}
	s := strings.TrimSpace(string(b))
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		s = s[1 : len(s)-1]
	}
	if s == "" {
		return "", fmt.Errorf("empty path")
	}
	return sys.ExpandHome(s)
}

// Use ANSI palette indices rather than RGB values so Base16 and other terminal
// themes supply status colors. Only local UI uses this formatter;
// PTY bytes and protocol streams must never be decorated. Reset foreground color
// immediately, including after prompts, so typed paths do not inherit it.
// Respect NO_COLOR, redirected output and TERM=dumb. Progress/status use stderr;
// listings remain plain stdout. Sanitize control characters in displayed paths
// and errors while preserving readable UTF-8; never filter the remote PTY stream.

type Color string

const (
	Info    Color = "36"
	Success Color = "32"
	Warning Color = "33"
	Error   Color = "31"
)

func ColorEnabled(f *os.File) bool {
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(f.Fd())) && supportsANSI(f)
}

func CleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '\ufffd'
		}
		return r
	}, s)
}

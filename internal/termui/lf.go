package termui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/jay-waves/sshore/internal/files/dirlist"
	"github.com/jay-waves/sshore/internal/shell"
)

// ErrLFManual requests a caller-owned path editor.
var ErrLFManual = errors.New("enter path manually")

// LFOptions supplies path operations and directory listings for a picker.
type LFOptions struct {
	Dir               string
	Directory         bool
	ReadDir           func(context.Context, string) ([]dirlist.Entry, error)
	ReadPair          func(ctx context.Context, current, parent string) (currentEntries []dirlist.Entry, currentErr error, parentEntries []dirlist.Entry, parentErr error)
	Join              func(string, string) string
	Parent            func(string) string
	Title             string
	Host              string
	ChooseDirectories bool
	Manual            bool
	LocalFiles        bool // The listed paths belong to this process's filesystem.
	BrowseAction      func(key byte, directory bool)
	BrowseTransfers   bool
	Validate          func(string) error
	Input             <-chan KeyInput
	Output            io.Writer
	console           *LocalConsole
}

type lfResult struct {
	path    string
	entries []dirlist.Entry
	err     error
}

type lfCacheEntry struct {
	entries []dirlist.Entry
	expires time.Time
}

type lfPane struct {
	entries []dirlist.Entry
	err     error
	loading bool
}

// PickLF presents parent, current and child directory lists. q and Ctrl-C
// return context.Canceled. ReadDir must honor its context for prompt cleanup.
func PickLF(ctx context.Context, options LFOptions) (string, error) {
	paths, err := pickLF(ctx, options, false, nil)
	if len(paths) == 0 {
		return "", err
	}
	return paths[0], err
}

// PickLFFiles adds Space selection for a batch of files.
func PickLFFiles(ctx context.Context, options LFOptions, validate func([]string) error) ([]string, error) {
	if options.Directory {
		return nil, errors.New("file multiselect requires a file picker")
	}
	return pickLF(ctx, options, true, validate)
}

func pickLF(ctx context.Context, options LFOptions, multi bool, validateFiles func([]string) error) (selected []string, err error) {
	if options.ReadDir == nil || options.Join == nil || options.Parent == nil || options.Dir == "" {
		return nil, errors.New("picker requires a directory, path operations and a directory reader")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	input := options.Input
	var out io.Writer
	var startTerminal func() error
	var stopTerminal func() error
	if input != nil {
		if options.Output == nil {
			return nil, errors.New("embedded picker requires an output writer")
		}
		out = options.Output
		defer func() {
			_, e := fmt.Fprint(out, "\x1b[?2004l\x1b[0m\x1b[?25h")
			err = errors.Join(err, e)
		}()
		if _, err := fmt.Fprint(out, "\x1b[?2004h"); err != nil {
			return nil, err
		}
	} else {
		if options.console == nil {
			return nil, errors.New("picker requires a local console or an input stream")
		}
		startTerminal = func() error {
			terminal, err := openPickerTerminal(ctx)
			if err != nil {
				return err
			}
			out, input = terminal.out, terminal.input
			stopTerminal = func() error { stopTerminal = nil; return terminal.close() }
			return nil
		}
		if err := startTerminal(); err != nil {
			return nil, err
		}
		defer func() {
			if stopTerminal != nil {
				err = errors.Join(err, stopTerminal())
			}
		}()
	}
	validateSelection := func(names []string) error {
		if options.Validate != nil {
			for _, name := range names {
				if err := options.Validate(name); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
			}
		}
		if validateFiles != nil {
			return validateFiles(names)
		}
		return nil
	}
	results := make(chan lfResult, 16)
	var panes [3]lfPane
	var paths [3]string
	current, index := options.Dir, 0
	var reveal string
	var validationErr error
	var marked []string
	cache := make(map[string]lfCacheEntry)
	inflight := make(map[string]bool)
	var hover <-chan time.Time
	send := func(dir string, entries []dirlist.Entry, err error) {
		select {
		case results <- lfResult{dir, entries, err}:
		case <-ctx.Done():
		}
	}
	cached := func(dir string) (lfCacheEntry, bool) {
		item, ok := cache[dir]
		return item, ok && time.Now().Before(item.expires)
	}
	load := func(pane int, dir string) {
		paths[pane] = dir
		if item, ok := cached(dir); ok {
			panes[pane] = lfPane{entries: item.entries}
			return
		}
		panes[pane] = lfPane{loading: true}
		if inflight[dir] {
			return
		}
		inflight[dir] = true
		go func() {
			entries, e := options.ReadDir(ctx, dir)
			send(dir, entries, e)
		}()
	}
	child := func() {
		paths[2] = ""
		panes[2] = lfPane{}
		hover = nil
		if index < len(panes[1].entries) && panes[1].entries[index].IsDir && panes[1].entries[index].Name != "." {
			paths[2] = options.Join(current, panes[1].entries[index].Name)
			panes[2].loading = true
			hover = time.After(100 * time.Millisecond)
		}
	}
	updateCursor := func() {
		if (options.Directory || options.BrowseAction != nil) && panes[1].err == nil {
			panes[1].entries = append([]dirlist.Entry{{Name: ".", IsDir: true}}, panes[1].entries...)
		}
		index = min(index, max(0, len(panes[1].entries)-1))
		if reveal != "" {
			for i, entry := range panes[1].entries {
				if options.Join(current, entry.Name) == reveal {
					index = i
					break
				}
			}
			reveal = ""
		}
		child()
	}
	refresh := func() {
		hover = nil
		paths = [3]string{}
		validationErr = nil
		panes = [3]lfPane{}
		parent := options.Parent(current)
		_, haveCurrent := cached(current)
		_, haveParent := cached(parent)
		if options.ReadPair != nil && parent != "" && parent != current &&
			!haveCurrent && !haveParent && !inflight[current] && !inflight[parent] {
			dir := current
			paths[0], paths[1] = parent, dir
			panes[0].loading, panes[1].loading = true, true
			inflight[parent], inflight[dir] = true, true
			go func() {
				entries, err, siblings, parentErr := options.ReadPair(ctx, dir, parent)
				send(dir, entries, err)
				send(parent, siblings, parentErr)
			}()
			return
		}
		load(1, current)
		if parent != current {
			load(0, parent)
		}
		if !panes[1].loading {
			updateCursor()
		}
	}
	refresh()
	color := ColorEnabled(os.Stdout)
	var decoder keyDecoder
	var awaitingG bool
	var searchPrompt rune
	cancelLayer := func() {
		if searchPrompt != 0 {
			searchPrompt = 0
			return
		}
		if len(marked) == 0 {
			return
		}
		marked, validationErr = nil, nil
	}
	var query []rune
	appendQuery := func(r rune) {
		if unicode.IsPrint(r) && len(query) < 1024 {
			query = append(query, r)
		}
	}
	var searchPattern string
	searchDirection := 1
	search := func(direction int) {
		next, ok := lfSearch(panes[1].entries, index, searchPattern, direction)
		if !ok {
			validationErr = fmt.Errorf("No match for %q", searchPattern)
			return
		}
		validationErr = nil
		index = next
		child()
	}
	move := func(next int) {
		next = max(0, min(next, len(panes[1].entries)-1))
		if next != index {
			index = next
			child()
		}
	}
	var display lfScreen
	runFileAction := func(action string, run func() error) error {
		if err := stopTerminal(); err != nil {
			return err
		}
		if err := options.console.close(); err != nil {
			return err
		}
		runErr := run()
		if err := options.console.open(); err != nil {
			return errors.Join(runErr, err)
		}
		if err := startTerminal(); err != nil {
			return errors.Join(runErr, err)
		}
		decoder = keyDecoder{}
		display.rows = nil
		validationErr = nil
		if runErr != nil {
			validationErr = fmt.Errorf("%s: %w", action, runErr)
		}
		return nil
	}
	size, resized := watchScreenSize(ctx)
	for {
		parentIndex := -1
		for i, entry := range panes[0].entries {
			if options.Join(paths[0], entry.Name) == current {
				parentIndex = i
				break
			}
		}
		prompt := ""
		if searchPrompt != 0 {
			prompt = string(searchPrompt) + string(query)
		}
		if err := display.draw(out, options, current, panes, parentIndex, index, size.width, size.height, validationErr, color, multi, marked, prompt); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-decoder.escape:
			decoder.finishEscape()
			cancelLayer()
		case size = <-resized:
		case <-hover:
			hover = nil
			if paths[2] != "" {
				load(2, paths[2])
			}
		case result := <-results:
			delete(inflight, result.path)
			if result.err == nil {
				result.entries = slices.Clone(result.entries)
				slices.SortFunc(result.entries, func(a, b dirlist.Entry) int {
					if a.IsDir != b.IsDir {
						if a.IsDir {
							return -1
						}
						return 1
					}
					return cmp.Compare(a.Name, b.Name)
				})
				// Keep a small per-picker cache; expired entries refresh on next use.
				if len(cache) >= 64 {
					var oldest string
					var expiry time.Time
					for path, item := range cache {
						if oldest == "" || item.expires.Before(expiry) {
							oldest, expiry = path, item.expires
						}
					}
					delete(cache, oldest)
				}
				cache[result.path] = lfCacheEntry{result.entries, time.Now().Add(2 * time.Second)}
			}
			for pane, path := range paths {
				if path != result.path || path == "" {
					continue
				}
				// A hovered preview must wait for the debounce before displaying.
				if pane == 2 && hover != nil {
					continue
				}
				panes[pane] = lfPane{entries: result.entries, err: result.err}
				if pane == 1 {
					updateCursor()
				}
			}
		case event, ok := <-input:
			if !ok {
				return nil, io.EOF
			}
			if event.Err != nil {
				return nil, event.Err
			}
			for event := range decoder.feed(event.Data) {
				key := event.value
				if event.pasted {
					awaitingG = false
					if searchPrompt != 0 {
						appendQuery(key)
					}
					continue
				}
				if key == 0x1b {
					cancelLayer()
					continue
				}
				if searchPrompt != 0 {
					switch key {
					case 3:
						searchPrompt = 0
					case '\b':
						if len(query) > 0 {
							query = query[:len(query)-1]
						}
					case '\n':
						if len(query) > 0 {
							searchPattern = string(query)
							searchDirection = 1
							if searchPrompt == '?' {
								searchDirection = -1
							}
							search(searchDirection)
						}
						searchPrompt = 0
					default:
						appendQuery(key)
					}
					continue
				}
				goToTop := key == 'g' && awaitingG
				awaitingG = key == 'g' && !goToTop
				display.notice = ""
				switch key {
				case 'y':
					if options.BrowseAction == nil || index >= len(panes[1].entries) {
						continue
					}
					name := options.Join(current, panes[1].entries[index].Name)
					if err := copyToClipboard(out, name); err != nil {
						return nil, err
					}
					validationErr = nil
					display.notice = "Copied path"
				case 's', 'c', 'm':
					if options.BrowseAction == nil || (key != 's' && !options.BrowseTransfers) || index >= len(panes[1].entries) {
						continue
					}
					entry := panes[1].entries[index]
					name := options.Join(current, entry.Name)
					if key == 's' && !entry.IsDir {
						name = current
					}
					if key == 'm' && !entry.IsDir {
						validationErr = errors.New("Choose a directory to mount")
						continue
					}
					options.BrowseAction(byte(key), entry.IsDir)
					return []string{name}, nil
				case '/', '?':
					searchPrompt = key
					query = query[:0]
					validationErr = nil
				case 'n', 'N':
					if searchPattern != "" {
						direction := searchDirection
						if key == 'N' {
							direction = -direction
						}
						search(direction)
					}
				case 'g':
					if goToTop {
						move(0)
					}
				case 'G':
					move(len(panes[1].entries) - 1)
				case 3, 'q':
					return nil, context.Canceled
				case ':':
					if options.Manual {
						return nil, ErrLFManual
					}
				case 'k', keyUp:
					move(index - 1)
				case 'j', keyDown:
					move(index + 1)
				case 'h', keyLeft:
					if parent := options.Parent(current); parent != current {
						reveal = current
						current, index = parent, 0
						refresh()
					}
				case 'e', 'i':
					if index >= len(panes[1].entries) || panes[1].entries[index].IsDir {
						continue
					}
					variable := "EDITOR"
					if key == 'i' {
						variable = "PAGER"
					}
					command, err := lfFileCommand(ctx, variable, options.Join(current, panes[1].entries[index].Name), options.LocalFiles && startTerminal != nil)
					validationErr = err
					if errors.Is(err, errLFNonText) && options.BrowseAction != nil {
						validationErr = fmt.Errorf("%w; use o to open with the system", err)
					}
					if err != nil {
						continue
					}
					if err := runFileAction(variable, command.Run); err != nil {
						return nil, err
					}
					if key == 'e' {
						delete(cache, current)
						editErr := validationErr
						refresh()
						validationErr = editErr
					}
				case ' ':
					if !multi || index >= len(panes[1].entries) || panes[1].entries[index].IsDir {
						continue
					}
					name := options.Join(current, panes[1].entries[index].Name)
					if i := slices.Index(marked, name); i >= 0 {
						marked = slices.Delete(marked, i, i+1)
					} else {
						marked = append(marked, name)
					}
					validationErr = nil
					move(index + 1)
				case 'o', 'l', keyRight, '\n':
					enter := key == '\n'
					if (options.BrowseAction != nil && enter) || (options.BrowseAction == nil && key == 'o') {
						continue
					}
					if enter && len(marked) > 0 {
						validationErr = validateSelection(marked)
						if validationErr == nil {
							return marked, nil
						}
						continue
					}
					if index >= len(panes[1].entries) {
						continue
					}
					entry := panes[1].entries[index]
					name := current
					if entry.Name != "." {
						name = options.Join(current, entry.Name)
					}
					if options.BrowseAction != nil && (key == 'o' || key == 'l' && !entry.IsDir) {
						if !options.LocalFiles || startTerminal == nil {
							validationErr = errors.New("System open is available for local paths")
							continue
						}
						if err := runFileAction("Open", func() error { return openLFPath(ctx, name) }); err != nil {
							return nil, err
						}
						continue
					}
					if entry.IsDir {
						if !(options.Directory || options.ChooseDirectories) || !enter {
							if name != current {
								reveal = ""
								current, index = name, 0
								refresh()
							}
							continue
						}
					} else if options.BrowseAction != nil || options.Directory || !enter && multi {
						continue
					}
					validationErr = validateSelection([]string{name})
					if validationErr != nil {
						continue
					}
					return []string{name}, nil
				}
			}
		}
	}
}

func lfFileCommand(ctx context.Context, variable, name string, local bool) (*exec.Cmd, error) {
	if !local {
		return nil, fmt.Errorf("%s requires a local picker with terminal access", variable)
	}
	name, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	if err := checkLFText(name); err != nil {
		return nil, err
	}
	value := strings.TrimSpace(os.Getenv(variable))
	if value == "" {
		return nil, fmt.Errorf("%s is not set", variable)
	}
	argv, err := shell.ParseCommand(value)
	if err != nil || len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("invalid %s command", variable)
	}
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], name)...)
	if cmd.Err != nil {
		return nil, fmt.Errorf("%s: %w", variable, cmd.Err)
	}
	cmd.Dir = filepath.Dir(name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

func lfSearch(entries []dirlist.Entry, index int, pattern string, direction int) (int, bool) {
	fold := strings.IndexFunc(pattern, unicode.IsUpper) < 0
	if fold {
		pattern = strings.ToLower(pattern)
	}
	for range entries {
		index = (index + direction + len(entries)) % len(entries)
		name := entries[index].Name
		if fold {
			name = strings.ToLower(name)
		}
		if strings.Contains(name, pattern) {
			return index, true
		}
	}
	return index, false
}

type lfScreen struct {
	listScreen
	directory    string
	firstVisible int
	notice       string
}

func (s *lfScreen) draw(out io.Writer, options LFOptions, dir string, panes [3]lfPane, parentIndex, selected, width, height int, validationErr error, color, multi bool, marked []string, prompt string) error {
	width, height = max(1, width), max(1, height)
	lines := make([]string, 0, height)
	header := Render(color, Heading, CleanText(options.Title))
	if options.Host != "" {
		if header != "" {
			header += Render(color, Muted, "  ·  ")
		}
		header += Render(color, Accent, CleanText(options.Host))
	}
	if height > 1 {
		lines = append(lines, ansi.Truncate(header, width-1, "…"))
	}
	if height > 2 {
		lines = append(lines, ansi.Truncate(CleanText(dir), width-1, "…"))
	}
	if height >= 5 {
		lines = append(lines, "")
	}
	showFileActions := options.BrowseAction != nil && options.LocalFiles && height >= 4
	// Keep the current directory visible even on very narrow terminals.
	widths := [3]int{width / 4, width / 3, width - width/4 - width/3 - 3}
	if width < 15 {
		widths = [3]int{0, width - 1, 0}
	}
	rows := height - len(lines) - 1
	if showFileActions {
		rows--
	}
	if s.directory != dir {
		s.directory, s.firstVisible = dir, 0
	}
	if rows > 0 {
		lastStart := max(0, len(panes[1].entries)-rows)
		s.firstVisible = max(selected-rows+1, min(s.firstVisible, selected, lastStart))
	}
	start := [3]int{max(0, parentIndex-rows+1), s.firstVisible, 0}
	var b strings.Builder
	for row := 0; row < rows; row++ {
		for p, w := range widths {
			if w <= 0 {
				continue
			}
			if p > 0 && widths[p-1] > 0 {
				b.WriteString(Render(color, Muted, "│"))
			}
			i := row + start[p]
			text := ""
			markedEntry := false
			style := Color("")
			if i < len(panes[p].entries) {
				entry := panes[p].entries[i]
				text = CleanText(entry.Name)
				if entry.IsDir {
					text += "/"
					style = Muted
					if p == 1 {
						style = Info
					}
				}
				markedEntry = multi && p == 1 && !entry.IsDir && len(marked) > 0 && slices.Contains(marked, options.Join(dir, entry.Name))
			} else if i == 0 && panes[p].loading {
				text = "…"
				style = Muted
			} else if i == 0 && panes[p].err != nil {
				text = CleanText(panes[p].err.Error())
				style = Warning
			}
			textWidth := w
			if w >= 8 && width >= 15 {
				b.WriteByte(' ')
				textWidth--
			}
			if multi && p == 1 && w >= 3 {
				if markedEntry {
					b.WriteString(Render(color, Success, "▌ "))
				} else {
					b.WriteString("  ")
				}
				textWidth -= 2
			}
			text = ansi.Truncate(text, textWidth, "…")
			padding := strings.Repeat(" ", max(0, textWidth-ansi.StringWidth(text)))
			if p == 1 && i == selected && i < len(panes[1].entries) {
				b.WriteString("\x1b[7m" + text + padding + "\x1b[27m")
				continue
			}
			if p == 0 && i == parentIndex {
				style = Heading
			}
			b.WriteString(Render(color, style, text))
			b.WriteString(padding)
		}
		lines = append(lines, b.String())
		b.Reset()
	}
	hint := "Enter Choose file  q Quit"
	if options.Directory {
		hint = "Enter Choose highlighted dir  l Open  q Cancel"
		if selected < len(panes[1].entries) && panes[1].entries[selected].Name == "." {
			hint = "Enter Choose current dir  l Open  q Cancel"
		}
	}
	if multi {
		hint = "Enter Choose file  Space Mark  / Search  q Cancel"
		if selected < len(panes[1].entries) && panes[1].entries[selected].IsDir {
			hint = "Enter Open directory  Space Mark files  / Search  q Cancel"
			if options.ChooseDirectories {
				hint = "Enter Copy directory recursively  l Open  q Cancel"
			}
		}
		if len(marked) > 0 {
			noun := "files"
			if len(marked) == 1 {
				noun = "file"
			}
			hint = fmt.Sprintf("%d %s marked  Enter Continue  Esc Clear  Space Toggle", len(marked), noun)
		}
	}
	if options.Manual {
		hint += "  : Type path"
	}
	if options.BrowseAction != nil {
		hint = "s Shell"
		if options.BrowseTransfers {
			hint += "  c Copy  m Mount"
		}
		if !showFileActions {
			hint += "  l Navigate"
		}
		hint += "  / Search  y Copy path  q Quit"
	}
	if showFileActions {
		const actions = "o Open  l Enter dir/Open file  e Editor  i Pager"
		lines = append(lines, ansi.Truncate(shortcutLine(color, actions), width-1, ""))
	}
	var footer string
	var cursorColumn int
	if prompt != "" {
		footer = ansi.Truncate(CleanText(prompt), width-1, "…")
		cursorColumn = ansi.StringWidth(footer) + 1
	} else if validationErr != nil {
		footer = Render(color, Warning, ansi.Truncate(CleanText(validationErr.Error()), width-1, "…"))
	} else if s.notice != "" {
		footer = Render(color, Muted, ansi.Truncate(s.notice, width-1, ""))
	} else {
		footer = ansi.Truncate(shortcutLine(color, hint), width-1, "")
	}
	return s.paint(out, width, append(lines, footer), cursorColumn)
}

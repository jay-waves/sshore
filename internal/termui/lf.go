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

	"github.com/charmbracelet/x/ansi"
	"github.com/jay-waves/sshore/internal/shell"
	"github.com/jay-waves/sshore/internal/term"
	xterm "golang.org/x/term"
)

// ErrLFManual requests a caller-owned path editor.
var ErrLFManual = errors.New("enter path manually")

// LFEntry is a directory entry; callers need not fetch file metadata.
type LFEntry struct {
	Name  string
	IsDir bool
}

// LFOptions supplies path operations and directory listings for a picker.
type LFOptions struct {
	Dir        string
	Directory  bool
	ReadDir    func(context.Context, string) ([]LFEntry, error)
	ReadPair   func(ctx context.Context, current, parent string) (currentEntries []LFEntry, currentErr error, parentEntries []LFEntry, parentErr error)
	Join       func(string, string) string
	Parent     func(string) string
	Title      string
	Host       string
	Automatic  bool
	Manual     bool
	InConsole  bool
	LocalFiles bool // The listed paths belong to this process's filesystem.
	Validate   func(string) error
	Input      <-chan LFInput
	Output     io.Writer
}

type lfResult struct {
	path    string
	entries []LFEntry
	err     error
}

type lfCacheEntry struct {
	entries []LFEntry
	expires time.Time
}

type lfPane struct {
	entries []LFEntry
	err     error
	loading bool
}

// LFInput carries input from an existing terminal owner.
type LFInput struct {
	Data []byte
	Err  error
}

// PickLF presents parent, current and child directory lists. q, Escape and Ctrl-C
// return context.Canceled. ReadDir must honor its context for prompt cleanup.
func PickLF(ctx context.Context, options LFOptions) (string, error) {
	paths, err := pickLF(ctx, options, nil)
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
	if validate == nil {
		validate = func([]string) error { return nil }
	}
	return pickLF(ctx, options, validate)
}

func pickLF(ctx context.Context, options LFOptions, validateFiles func([]string) error) (selected []string, err error) {
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
		defer func() { _, e := fmt.Fprint(out, "\x1b[0m\x1b[?25h"); err = errors.Join(err, e) }()
	} else {
		if !xterm.IsTerminal(int(os.Stdin.Fd())) || !xterm.IsTerminal(int(os.Stdout.Fd())) {
			return nil, ErrTerminalRequired
		}
		startTerminal = func() error {
			restore, err := term.RawConsole()
			if err != nil {
				return err
			}
			writer, flush := term.OutputWriter()
			out = writer
			var screen term.InputState
			if !options.InConsole {
				if err := screen.BeginConsole(out); err != nil {
					restore()
					return errors.Join(err, flush())
				}
			}
			keyCtx, cancelKeys := context.WithCancel(ctx)
			events := make(chan LFInput, 32)
			stopKeys, err := term.StartKeyboard(keyCtx, func(data []byte, err error) {
				select {
				case events <- LFInput{data, err}:
				case <-keyCtx.Done():
				}
			})
			stopTerminal = func() error {
				cancelKeys()
				if stopKeys != nil {
					stopKeys()
				}
				var err error
				if options.InConsole {
					_, err = fmt.Fprint(out, "\x1b[0m\x1b[?25h")
				} else {
					err = screen.EndConsole(out)
				}
				err = errors.Join(err, flush())
				restore()
				stopTerminal = nil
				return err
			}
			if err != nil {
				return errors.Join(err, stopTerminal())
			}
			input = events
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
	send := func(dir string, entries []LFEntry, err error) {
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
		if options.Directory && panes[1].err == nil {
			panes[1].entries = append([]LFEntry{{Name: ".", IsDir: true}}, panes[1].entries...)
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
	var pending []byte
	var awaitingG bool
	var escape <-chan time.Time
	resize := time.NewTicker(100 * time.Millisecond)
	defer resize.Stop()
	for {
		width, height, sizeErr := xterm.GetSize(int(os.Stdout.Fd()))
		if sizeErr != nil {
			width, height = 80, 24
		}
		parentIndex := -1
		for i, entry := range panes[0].entries {
			if options.Join(paths[0], entry.Name) == current {
				parentIndex = i
				break
			}
		}
		if err := lfDraw(out, options, current, panes, parentIndex, index, width, height, validationErr, color, validateFiles != nil, marked); err != nil {
			return nil, err
		}
	waitForEvent:
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-escape:
			return nil, context.Canceled
		case <-resize.C:
			newWidth, newHeight, err := xterm.GetSize(int(os.Stdout.Fd()))
			if err != nil || newWidth == width && newHeight == height {
				goto waitForEvent
			}
		case <-hover:
			hover = nil
			if paths[2] != "" {
				load(2, paths[2])
			}
		case result := <-results:
			delete(inflight, result.path)
			if result.err == nil {
				result.entries = slices.Clone(result.entries)
				slices.SortFunc(result.entries, func(a, b LFEntry) int {
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
			pending = append(pending, event.Data...)
			for len(pending) > 0 {
				key := pending[0]
				if key == 0x1b {
					if len(pending) == 1 {
						escape = time.After(50 * time.Millisecond)
						break
					}
					escape = nil
					if pending[1] != '[' && pending[1] != 'O' {
						return nil, context.Canceled
					}
					if len(pending) < 3 {
						break
					}
					end := 2
					if pending[1] == '[' {
						for end < len(pending) && pending[end] >= 0x20 && pending[end] <= 0x3f {
							end++
						}
						if end == len(pending) {
							break
						}
					}
					key = 0
					if end == 2 {
						switch pending[2] {
						case 'A':
							key = 'k'
						case 'B':
							key = 'j'
						case 'C':
							key = 'l'
						case 'D':
							key = 'h'
						}
					}
					pending = pending[end+1:]
				} else {
					pending = pending[1:]
				}
				goToTop := key == 'g' && awaitingG
				awaitingG = key == 'g' && !goToTop
				switch key {
				case 'g':
					if goToTop && index != 0 {
						index = 0
						child()
					}
				case 'G':
					last := max(0, len(panes[1].entries)-1)
					if index != last {
						index = last
						child()
					}
				case 3, 'q':
					return nil, context.Canceled
				case ':':
					if options.Manual {
						return nil, ErrLFManual
					}
				case 'a':
					if options.Directory && options.Automatic {
						return nil, nil
					}
				case 'k':
					if index > 0 {
						index--
						child()
					}
				case 'j':
					if index+1 < len(panes[1].entries) {
						index++
						child()
					}
				case 'h':
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
					if err != nil {
						continue
					}
					if err := stopTerminal(); err != nil {
						return nil, err
					}
					runErr := command.Run()
					if err := startTerminal(); err != nil {
						return nil, errors.Join(runErr, err)
					}
					pending = nil
					if key == 'e' {
						delete(cache, current)
						refresh()
					}
					if runErr != nil {
						validationErr = fmt.Errorf("%s: %w", variable, runErr)
					}
				case ' ':
					if validateFiles == nil || index >= len(panes[1].entries) || panes[1].entries[index].IsDir {
						continue
					}
					name := options.Join(current, panes[1].entries[index].Name)
					if i := slices.Index(marked, name); i >= 0 {
						marked = slices.Delete(marked, i, i+1)
					} else {
						marked = append(marked, name)
					}
					validationErr = nil
					if index+1 < len(panes[1].entries) {
						index++
						child()
					}
				case 'l', '\r', '\n':
					enter := key != 'l'
					if enter && len(marked) > 0 {
						validationErr = validateFiles(marked)
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
					if entry.IsDir {
						if !options.Directory || !enter {
							if name != current {
								reveal = ""
								current, index = name, 0
								refresh()
							}
							continue
						}
					} else if options.Directory || !enter && validateFiles != nil {
						continue
					}
					if options.Validate != nil {
						validationErr = options.Validate(name)
						if validationErr != nil {
							continue
						}
					}
					if validateFiles != nil {
						validationErr = validateFiles([]string{name})
						if validationErr != nil {
							continue
						}
					}
					return []string{name}, nil
				}
			}
		}
	}
}

func lfFileCommand(ctx context.Context, variable, name string, local bool) (*exec.Cmd, error) {
	value := strings.TrimSpace(os.Getenv(variable))
	if value == "" {
		return nil, fmt.Errorf("%s is not set", variable)
	}
	if !local {
		return nil, fmt.Errorf("%s requires a local picker with terminal access", variable)
	}
	argv, err := shell.ParseCommand(value)
	if err != nil || len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("invalid %s command", variable)
	}
	name, err = filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], name)...)
	if cmd.Err != nil {
		return nil, fmt.Errorf("%s: %w", variable, cmd.Err)
	}
	cmd.Dir = filepath.Dir(name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

func lfDraw(out io.Writer, options LFOptions, dir string, panes [3]lfPane, parentIndex, selected, width, height int, validationErr error, color, multi bool, marked []string) error {
	width, height = max(3, width), max(5, height)
	var b strings.Builder
	b.WriteString("\x1b[0m\x1b[H\x1b[2J\x1b[?25l")
	header := Render(color, Heading, CleanText(options.Title))
	if options.Host != "" {
		if header != "" {
			header += "  ·  "
		}
		header += Render(color, Accent, CleanText(options.Host))
	}
	b.WriteString(ansi.Truncate(header, width-1, "…"))
	b.WriteString("\r\n")
	b.WriteString(ansi.Truncate(CleanText(dir), width-1, "…"))
	b.WriteString("\r\n\r\n")
	// Keep the current directory visible even on very narrow terminals.
	widths := [3]int{width / 4, width / 3, width - width/4 - width/3 - 3}
	if width < 15 {
		widths = [3]int{0, width - 1, 0}
	}
	rows := height - 4
	start := [3]int{max(0, parentIndex-rows+1), max(0, selected-rows+1), 0}
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
			style := Color("")
			if i < len(panes[p].entries) {
				entry := panes[p].entries[i]
				text = CleanText(entry.Name)
				if entry.IsDir {
					text += "/"
					style = Info
				}
				if multi && p == 1 && slices.Contains(marked, options.Join(dir, entry.Name)) {
					text = "* " + text
				}
			} else if i == 0 && panes[p].loading {
				text = "…"
				style = Muted
			} else if i == 0 && panes[p].err != nil {
				text = CleanText(panes[p].err.Error())
				style = Warning
			}
			text = ansi.Truncate(text, w, "…")
			padding := strings.Repeat(" ", max(0, w-ansi.StringWidth(text)))
			if p == 1 && i == selected && i < len(panes[p].entries) {
				b.WriteString("\x1b[7m" + text + padding + "\x1b[27m")
				continue
			}
			if p == 0 && i == parentIndex {
				style = Heading
			}
			b.WriteString(Render(color, style, text))
			b.WriteString(padding)
		}
		if row+1 < rows {
			b.WriteString("\r\n")
		}
	}
	hint := "Enter Choose file  q/Esc Cancel"
	if options.Directory {
		hint = "Enter Choose highlighted dir  l Open  q Cancel"
		if selected < len(panes[1].entries) && panes[1].entries[selected].Name == "." {
			hint = "Enter Choose current dir  l Open  q Cancel"
		}
		if options.Automatic {
			hint += "  a Automatic"
		}
	}
	if multi {
		hint = "Space Mark  Enter Choose file  q/Esc Cancel"
		if len(marked) > 0 {
			hint = fmt.Sprintf("Space Mark  Enter Copy %d file(s)  l Open  q/Esc Cancel", len(marked))
		}
	}
	if options.Manual {
		hint += "  : Type path"
	}
	b.WriteString("\r\n")
	if validationErr != nil {
		b.WriteString(Render(color, Warning, ansi.Truncate(CleanText(validationErr.Error()), width-1, "…")))
	} else {
		b.WriteString(Render(color, Muted, ansi.Truncate(hint, width-1, "")))
	}
	_, err := fmt.Fprint(out, b.String())
	return err
}

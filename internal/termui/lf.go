package termui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
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
	Dir       string
	Directory bool
	ReadDir   func(context.Context, string) ([]LFEntry, error)
	ReadPair  func(ctx context.Context, current, parent string) (currentEntries []LFEntry, currentErr error, parentEntries []LFEntry, parentErr error)
	Join      func(string, string) string
	Parent    func(string) string
	Title     string
	Host      string
	Automatic bool
	Manual    bool
	InConsole bool
	Validate  func(string) error
	Input     <-chan LFInput
	Output    io.Writer
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
func PickLF(ctx context.Context, options LFOptions) (selected string, err error) {
	if options.ReadDir == nil || options.Join == nil || options.Parent == nil || options.Dir == "" {
		return "", errors.New("picker requires a directory, path operations and a directory reader")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	input := options.Input
	var out io.Writer
	if input != nil {
		if options.Output == nil {
			return "", errors.New("embedded picker requires an output writer")
		}
		out = options.Output
		defer func() { _, e := fmt.Fprint(out, "\x1b[0m\x1b[?25h"); err = errors.Join(err, e) }()
	} else {
		if !xterm.IsTerminal(int(os.Stdin.Fd())) || !xterm.IsTerminal(int(os.Stdout.Fd())) {
			return "", ErrTerminalRequired
		}
		restore, e := term.RawConsole()
		if e != nil {
			return "", e
		}
		defer restore()
		var flush func() error
		out, flush = term.OutputWriter()
		defer func() { err = errors.Join(err, flush()) }()
		var screen term.InputState
		if options.InConsole {
			defer func() { _, e := fmt.Fprint(out, "\x1b[0m\x1b[?25h"); err = errors.Join(err, e) }()
		} else {
			if e := screen.BeginConsole(out); e != nil {
				return "", e
			}
			defer func() { err = errors.Join(err, screen.EndConsole(out)) }()
		}
		events := make(chan LFInput, 32)
		stop, e := term.StartKeyboard(ctx, func(data []byte, e error) {
			select {
			case events <- LFInput{data, e}:
			case <-ctx.Done():
			}
		})
		if e != nil {
			return "", e
		}
		defer stop()
		input = events
	}
	results := make(chan lfResult, 16)
	var panes [3]lfPane
	var paths [3]string
	current, index := options.Dir, 0
	var reveal string
	var validationErr error
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
		if index < len(panes[1].entries) && panes[1].entries[index].IsDir {
			paths[2] = options.Join(current, panes[1].entries[index].Name)
			panes[2].loading = true
			hover = time.After(100 * time.Millisecond)
		}
	}
	updateCursor := func() {
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
		if err := lfDraw(out, options, current, panes, parentIndex, index, width, height, validationErr, color); err != nil {
			return "", err
		}
	waitForEvent:
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-escape:
			return "", context.Canceled
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
				return "", io.EOF
			}
			if event.Err != nil {
				return "", event.Err
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
						return "", context.Canceled
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
				switch key {
				case 3, 'q':
					return "", context.Canceled
				case ':':
					if options.Manual {
						return "", ErrLFManual
					}
				case 'a':
					if options.Directory && options.Automatic {
						return "", nil
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
				case 'l', '\r', '\n':
					if (key == '\r' || key == '\n') && options.Directory {
						if panes[1].loading || panes[1].err != nil {
							continue
						}
						if options.Validate != nil {
							validationErr = options.Validate(current)
							if validationErr != nil {
								continue
							}
						}
						return current, nil
					}
					if index >= len(panes[1].entries) {
						continue
					}
					entry := panes[1].entries[index]
					path := options.Join(current, entry.Name)
					if entry.IsDir {
						reveal = ""
						current, index = path, 0
						refresh()
					} else if !options.Directory {
						if options.Validate != nil {
							validationErr = options.Validate(path)
							if validationErr != nil {
								continue
							}
						}
						return path, nil
					}
				}
			}
		}
	}
}

func lfDraw(out io.Writer, options LFOptions, dir string, panes [3]lfPane, parentIndex, selected, width, height int, validationErr error, color bool) error {
	width, height = max(3, width), max(3, height)
	var b strings.Builder
	b.WriteString("\x1b[0m\x1b[H\x1b[2J\x1b[?25l")
	line := CleanText(dir)
	if options.Host != "" {
		line = Render(color, Accent, CleanText(options.Host)) + "  " + line
		if options.Title != "" {
			line += Render(color, Muted, "  ·  ") + Render(color, Heading, CleanText(options.Title))
		}
	} else if options.Title != "" {
		line = Render(color, Heading, CleanText(options.Title)) + "  " + line
	}
	b.WriteString(ansi.Truncate(line, width-1, "…"))
	b.WriteString("\r\n")
	// Keep the current directory visible even on very narrow terminals.
	widths := [3]int{width / 4, width / 3, width - width/4 - width/3 - 3}
	if width < 15 {
		widths = [3]int{0, width - 1, 0}
	}
	rows := height - 2
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
		hint = "Enter Choose directory  q/Esc Cancel"
		if options.Automatic {
			hint += "  a Automatic"
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

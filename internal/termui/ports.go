package termui

import (
	"context"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/jay-waves/sshore/internal/term"
	xterm "golang.org/x/term"
)

// PickPort returns the highlighted row, or -1 for manual entry.
func (c *LocalConsole) PickPort(host string, labels []string) (int, error) {
	restore, err := term.RawConsole()
	if err != nil {
		return -1, err
	}
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	keys := make(chan rune, 32)
	failed := make(chan error, 1)
	key := func(r rune) {
		select {
		case keys <- r:
		case <-ctx.Done():
		}
	}
	const (
		up   rune = -1
		down rune = -2
	)
	parser := ansi.NewParser()
	parser.SetHandler(ansi.Handler{Print: key, Execute: func(b byte) { key(rune(b)) }, HandleCsi: func(cmd ansi.Cmd, _ ansi.Params) {
		switch cmd {
		case 'A':
			key(up)
		case 'B':
			key(down)
		}
	}})
	stop, err := term.StartKeyboard(ctx, func(data []byte, err error) {
		if err != nil {
			select {
			case failed <- err:
			case <-ctx.Done():
			}
			return
		}
		for _, b := range data {
			parser.Advance(b)
		}
	})
	if err != nil {
		cancel()
		return -1, err
	}
	defer func() { cancel(); stop() }()
	var screen listScreen
	index := 0
	resize := time.NewTicker(250 * time.Millisecond)
	defer resize.Stop()
	var filtering bool
	var query []rune
	visible := make([]int, 0, len(labels))
	filter := func() {
		visible = visible[:0]
		needle := strings.ToLower(string(query))
		for i, label := range labels {
			if strings.Contains(strings.ToLower(label), needle) {
				visible = append(visible, i)
			}
		}
		index = 0
	}
	filter()
	color := ColorEnabled(os.Stdout)
	for {
		w, h, err := xterm.GetSize(int(os.Stdout.Fd()))
		if err != nil || w == 0 || h == 0 {
			w, h = 80, 24
		}
		lines := []string{Render(color, Heading, "Listening TCP ports") + "  ·  " + Render(color, Accent, CleanText(host)), Render(color, Muted, "PORT    ADDRESS          PROCESS")}
		size := max(1, h-3)
		first := max(0, index-size+1)
		for i := first; i < min(len(visible), first+size); i++ {
			row := ansi.Truncate(CleanText(labels[visible[i]]), w-1, "…")
			if i == index {
				row = "\x1b[7m" + row + "\x1b[0m"
			}
			lines = append(lines, row)
		}
		if len(visible) == 0 {
			message := "No listeners"
			if filtering {
				message = "No matches"
			}
			lines = append(lines, message)
		}
		hint := "↑/↓ Move  Enter Forward  / Filter  : Enter port  q Quit"
		if filtering {
			hint = "/" + string(query)
		}
		lines = append(lines, Render(color, Muted, hint))
		for len(lines) < min(len(screen.rows), h) {
			lines = append(lines, "")
		}
		for i := range lines {
			lines[i] = ansi.Truncate(lines[i], w-1, "…")
		}
		if err := screen.paint(os.Stdout, w, lines, 0); err != nil {
			return -1, err
		}
		select {
		case <-resize.C:
		case err := <-failed:
			return -1, err
		case r := <-keys:
			if filtering && r != up && r != down {
				switch r {
				case 3:
					filtering = false
					query = nil
				case '\r', '\n':
					if len(visible) > 0 {
						return visible[index], nil
					}
					continue
				case 8, 127:
					if len(query) > 0 {
						query = query[:len(query)-1]
					}
				default:
					if unicode.IsPrint(r) && len(query) < 128 {
						query = append(query, r)
					}
				}
				filter()
				continue
			}
			switch r {
			case '/':
				filtering = true
			case 'q', 3:
				return -1, context.Canceled
			case ':':
				return -1, nil
			case 'j', down:
				index = min(max(0, len(visible)-1), index+1)
			case 'k', up:
				index = max(0, index-1)
			case '\r', '\n':
				if len(visible) > 0 {
					return visible[index], nil
				}
			}
		}
	}
}

package termui

import (
	"context"
	"errors"
	"os"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// PickPort browses listeners and returns a selection in forwarding mode.
// A selection of -1 requests manual port entry.
func (c *LocalConsole) PickPort(host string, labels []string, forward bool) (selected int, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	terminal, err := openPickerTerminal(ctx)
	if err != nil {
		return -1, err
	}
	defer func() { err = errors.Join(err, terminal.close()) }()
	var decoder keyDecoder
	var screen listScreen
	index := 0
	size, resized := watchScreenSize(ctx)
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
		w, h := size.width, size.height
		lines := []string{Render(color, Heading, "Bound ports") + "  ·  " + Render(color, Accent, CleanText(host)), Render(color, Muted, "PROTO  PORT    ADDRESS          PROCESS")}
		page := max(1, h-4)
		first := max(0, index-page+1)
		for i := first; i < min(len(visible), first+page); i++ {
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
		lines = append(lines, Render(color, Muted, "Some process names may require administrator privileges."))
		hint := "↑/↓ Move  / Filter  q Quit"
		if forward {
			hint = "↑/↓ Move  Enter Forward  / Filter  : Enter port  q Quit"
		}
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
		if err := screen.paint(terminal.out, w, lines, 0); err != nil {
			return -1, err
		}
		select {
		case size = <-resized:
		case <-decoder.escape:
			decoder.finishEscape()
		case input := <-terminal.input:
			if input.Err != nil {
				return -1, input.Err
			}
			for event := range decoder.feed(input.Data) {
				r := event.value
				if event.pasted {
					if filtering && unicode.IsPrint(r) && len(query) < 128 {
						query = append(query, r)
						filter()
					}
					continue
				}
				if filtering && r != keyUp && r != keyDown {
					switch r {
					case 3:
						filtering = false
						query = nil
					case '\n':
						if forward {
							if len(visible) > 0 {
								return visible[index], nil
							}
						} else {
							filtering = false
						}
						continue
					case '\b':
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
					if forward {
						return -1, nil
					}
				case 'j', keyDown:
					index = min(max(0, len(visible)-1), index+1)
				case 'k', keyUp:
					index = max(0, index-1)
				case '\n':
					if forward && len(visible) > 0 {
						return visible[index], nil
					}
				}
			}
		}
	}
}

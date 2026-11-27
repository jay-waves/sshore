package termui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	xterm "golang.org/x/term"
)

type screenSize struct{ width, height int }

func terminalSize() screenSize {
	w, h, err := xterm.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return screenSize{80, 24}
	}
	return screenSize{w, h}
}

func watchScreenSize(ctx context.Context) (screenSize, <-chan screenSize) {
	initial := terminalSize()
	changes := make(chan screenSize)
	go func() {
		size := initial
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				next := terminalSize()
				if next != size {
					select {
					case changes <- next:
						size = next
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return initial, changes
}

type listScreen struct {
	rows         []string
	width        int
	cursorColumn int
}

// Paint only changed rows. Erasing to the right of each replacement removes
// old text without blanking the screen between keystrokes.
func (s *listScreen) paint(out io.Writer, width int, rows []string, cursorColumn int) error {
	full := len(rows) != len(s.rows) || width != s.width
	var update strings.Builder
	if full || s.cursorColumn != 0 && cursorColumn == 0 {
		update.WriteString("\x1b[0m\x1b[?25l")
	}
	for i, row := range rows {
		if full || row != s.rows[i] {
			fmt.Fprintf(&update, "\x1b[%d;1H%s\x1b[K", i+1, row)
		}
	}
	if cursorColumn != 0 && (update.Len() > 0 || s.cursorColumn != cursorColumn) {
		fmt.Fprintf(&update, "\x1b[%d;%dH\x1b[?25h", len(rows), cursorColumn)
	}
	if update.Len() == 0 {
		return nil
	}
	if _, err := io.WriteString(out, update.String()); err != nil {
		return err
	}
	s.rows, s.width, s.cursorColumn = rows, width, cursorColumn
	return nil
}

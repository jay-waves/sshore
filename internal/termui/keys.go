package termui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
	"github.com/jay-waves/sshore/internal/term"
)

// KeyInput carries bytes from the current owner of the terminal.
type KeyInput struct {
	Data []byte
	Err  error
}

const (
	keyUp rune = -1 - iota
	keyDown
	keyRight
	keyLeft
)
const pasteStart, pasteEnd = "\x1b[200~", "\x1b[201~"

type pickerKey struct {
	value  rune
	pasted bool
}

type keyDecoder struct {
	pending         []byte
	pasted, afterCR bool
	escape          <-chan time.Time
	opaque          *ansi.Parser
}

func (d *keyDecoder) feed(data []byte) iter.Seq[pickerKey] {
	return func(yield func(pickerKey) bool) {
		d.pending = append(d.pending, data...)
		for {
			key, ok := d.next()
			if !ok || !yield(key) {
				return
			}
		}
	}
}

// next retains incomplete UTF-8 and terminal sequences across input reads.
// Pasted text is literal, even when it contains a command key or newline.
func (d *keyDecoder) next() (pickerKey, bool) {
	for len(d.pending) > 0 {
		p := d.pending
		if d.opaque != nil {
			d.opaque.Advance(p[0])
			d.pending = p[1:]
			if d.opaque.State() == parser.GroundState {
				d.opaque = nil
			}
			continue
		}
		if d.pasted {
			if bytes.HasPrefix(p, []byte(pasteEnd)) {
				d.pasted = false
				d.pending = p[len(pasteEnd):]
				continue
			}
			if bytes.HasPrefix([]byte(pasteEnd), p) {
				break
			}
		} else if p[0] == 0x1b {
			d.afterCR = false
			if len(p) == 1 {
				d.escape = time.After(50 * time.Millisecond)
				break
			}
			d.escape = nil
			switch p[1] {
			case '[', 'O':
				end := 2
				for end < len(p) && p[end] >= 0x20 && p[end] <= 0x3f {
					end++
				}
				if end == len(p) {
					return pickerKey{}, false
				}
				d.pending = p[end+1:]
				if string(p[:end+1]) == pasteStart {
					d.pasted = true
					continue
				}
				if end == 2 && p[end] >= 'A' && p[end] <= 'D' {
					return pickerKey{value: [4]rune{keyUp, keyDown, keyRight, keyLeft}[p[end]-'A']}, true
				}
				continue
			case ']', 'P', 'X', '^', '_':
				d.opaque = new(ansi.Parser) // Discard terminal replies without storing their payload.
				d.opaque.Advance(0x1b)
				d.pending = p[1:]
				continue
			}
			d.pending = p[1:]
			return pickerKey{value: 0x1b}, true
		}
		if !utf8.FullRune(p) {
			break
		}
		r, n := utf8.DecodeRune(p)
		d.pending = p[n:]
		if d.afterCR && r == '\n' {
			d.afterCR = false
			continue
		}
		d.afterCR = !d.pasted && r == '\r'
		if !d.pasted {
			switch r {
			case '\r':
				r = '\n'
			case 127:
				r = '\b'
			}
		}
		return pickerKey{value: r, pasted: d.pasted}, true
	}
	return pickerKey{}, false
}

// A lone Escape has timed out rather than introducing a terminal sequence.
func (d *keyDecoder) finishEscape() { d.pending = nil; d.escape = nil }

type pickerTerminal struct {
	input <-chan KeyInput
	out   io.Writer
	close func() error
}

func openPickerTerminal(ctx context.Context) (pickerTerminal, error) {
	restore, err := term.RawConsole()
	if err != nil {
		return pickerTerminal{}, err
	}
	out, flush := term.OutputWriter()
	keys, cancel := context.WithCancel(ctx)
	input := make(chan KeyInput, 32)
	stop, err := term.StartKeyboard(keys, func(data []byte, err error) {
		select {
		case input <- KeyInput{data, err}:
		case <-keys.Done():
		}
	})
	closeTerminal := func() error {
		cancel()
		if stop != nil {
			stop()
		}
		_, err := fmt.Fprint(out, "\x1b[?2004l\x1b[0m\x1b[?25h")
		err = errors.Join(err, flush())
		restore()
		return err
	}
	if err == nil {
		_, err = fmt.Fprint(out, "\x1b[?2004h")
	}
	if err != nil {
		return pickerTerminal{}, errors.Join(err, closeTerminal())
	}
	return pickerTerminal{input, out, closeTerminal}, nil
}

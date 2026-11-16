package tmux

import (
	"bytes"
	"slices"
	"sort"
	"strconv"
)

type KeyAction uint8

const (
	KeyNone KeyAction = iota
	KeyDetach
	KeyRedraw
	KeyInfo
	KeyHelp
	KeyNew
	KeyNext
	KeyPrevious
	KeyLast
	KeyChoose
)

// KeyEvent is either opaque terminal input or a local command, in input order.
type KeyEvent struct {
	Data   []byte
	Action KeyAction
}

// Keys retains prefix and paste delimiter state across arbitrary input reads.
// The zero value enables shortcuts. Bypass forwards all bytes unchanged.
type Keys struct {
	Bypass        bool
	prefix, paste bool
	marker        []byte
}

func (k *Keys) Feed(data []byte) []KeyEvent {
	if len(data) == 0 {
		return nil
	}
	if k.Bypass {
		return []KeyEvent{{Data: append([]byte(nil), data...)}}
	}
	var events []KeyEvent
	var out []byte
	flush := func() {
		if len(out) > 0 {
			events = append(events, KeyEvent{Data: out})
			out = nil
		}
	}
	key := func(c byte) bool {
		if k.paste {
			out = append(out, c)
			return false
		}
		if k.prefix {
			k.prefix = false
			var action KeyAction
			switch c {
			case 2:
				out = append(out, 2)
				return false
			case 'c':
				action = KeyNew
			case 'n':
				action = KeyNext
			case 'p':
				action = KeyPrevious
			case 'l':
				action = KeyLast
			case 's', 'w':
				action = KeyChoose
			case 'd':
				action = KeyDetach
			case 'r':
				action = KeyRedraw
			case 'i':
				action = KeyInfo
			case '?':
				action = KeyHelp
			default:
				out = append(out, 2, c)
				return false
			}
			flush()
			events = append(events, KeyEvent{Action: action})
			return action == KeyDetach || action.IsSessionCommand()
		}
		if c == 2 {
			k.prefix = true
			return false
		}

		out = append(out, c)
		return false
	}
	for _, c := range data {
		target := []byte("\x1b[200~")
		if k.paste {
			target = []byte("\x1b[201~")
		}
		if len(k.marker) > 0 || c == 0x1b {
			k.marker = append(k.marker, c)
			if !bytes.HasPrefix(target, k.marker) {
				k.marker = nil
				if c == 0x1b {
					k.marker = append(k.marker, c)
				}
			}
		}
		complete := bytes.Equal(target, k.marker)
		// Forward escape bytes immediately: a lone Escape must never wait for input.
		if key(c) {
			return events
		}
		if complete {
			k.marker = nil
			k.paste = !k.paste
		}
	}

	flush()
	return events
}

func (a KeyAction) IsSessionCommand() bool { return a >= KeyNew && a <= KeyChoose }

// SessionOrder is stable, includes current, and never changes caller-owned data.
func SessionOrder(current string, names []string) []string {
	all := append([]string{current}, names...)
	sort.Strings(all)
	return slices.Compact(all)
}
func AdjacentSession(current string, names []string, step int) string {
	all := SessionOrder(current, names)
	i := sort.SearchStrings(all, current)
	return all[(i+step%len(all)+len(all))%len(all)]
}

type SessionChooser struct {
	Names  []string
	Number string
}

// Feed consumes modal input only; trailing bytes after selection are discarded.
func (c *SessionChooser) Feed(data []byte) (selected string, done bool) {
	for _, b := range data {
		switch {
		case b == 27 || b == 3:
			return "", true
		case b == 13 || b == 10:
			if c.Number == "" {
				return "", true
			}
			n, err := strconv.Atoi(c.Number)
			if err == nil && n > 0 && n <= len(c.Names) {
				return c.Names[n-1], true
			}
			c.Number = ""
		case b == 127 || b == 8:
			if len(c.Number) > 0 {
				c.Number = c.Number[:len(c.Number)-1]
			}
		case b >= '0' && b <= '9':
			if len(c.Number) < 3 {
				c.Number += string(b)
			}
		}
	}
	return "", false
}

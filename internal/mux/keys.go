package mux

import "bytes"

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
		if c == 0x1d {
			flush()
			events = append(events, KeyEvent{Action: KeyDetach})
			return true
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

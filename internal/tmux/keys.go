package tmux

import (
	"bytes"
	"slices"
	"sort"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
)

type KeyAction uint8

const (
	KeyNone KeyAction = iota
	KeyDetach
	KeyRedraw
	KeyNew
	KeyNext
	KeyPrevious
	KeyLast
)

// KeyEvent is either opaque terminal input or a local command, in input order.
type KeyEvent struct {
	Data   []byte
	Action KeyAction
}

// Keys retains sequence, prefix and paste state across arbitrary input reads.
// The zero value enables shortcuts. Bypass forwards all bytes unchanged.
type Keys struct {
	opaque         byte
	opaqueEscape   bool
	mouseTail      int
	reports        []byte
	forwardReports []byte
	Bypass         bool
	Interrupted    bool // Ctrl+C press/repeat outside bracketed paste in the latest Feed.
	prefix         []byte
	paste          bool
	marker         []byte
	parser         *ansi.Parser
	sequence       []byte
	streaming      bool
	consumed       map[int]bool
}

// Pending reports whether an ambiguous Escape/CSI needs a bounded input timeout.
func (k *Keys) Pending() bool { return len(k.sequence) > 0 }

// FlushPending forwards an incomplete sequence verbatim. Continue observing its
// framing so a late tail cannot become a shortcut. A Ctrl+B prefix has no timeout.
func (k *Keys) FlushPending() []KeyEvent {
	if len(k.sequence) == 0 {
		return nil
	}
	data := append(append(append([]byte(nil), k.prefix...), k.reports...), k.sequence...)
	delete(k.consumed, 'b')
	k.reports, k.forwardReports = nil, nil
	k.prefix, k.sequence = nil, nil
	k.streaming = true
	// A lone Escape is now a complete key, rather than an unfinished sequence.
	if k.parser.State() == parser.EscapeState {
		k.parser.Reset()
		k.streaming = false
	}
	return []KeyEvent{{Data: data}}
}

func (k *Keys) Feed(data []byte) []KeyEvent {
	k.Interrupted = false
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
	literal := func(raw []byte) {
		if len(k.prefix) > 0 {
			delete(k.consumed, 'b')
		}
		out = append(out, k.prefix...)
		out = append(out, k.reports...)
		k.prefix, k.reports, k.forwardReports = nil, nil, nil
		out = append(out, raw...)
	}
	key := func(raw []byte, code, mods, event int) bool {
		// Release reports must not consume the prefix. Suppress reports for keys
		// consumed locally, including modifier changes between press and release.
		if event == 3 {
			consumed := k.consumed[code]
			delete(k.consumed, code)
			if len(k.prefix) > 0 {
				if len(k.reports)+len(raw) > 128 {
					literal(raw)
				} else {
					k.reports = append(k.reports, raw...)
					if !consumed {
						k.forwardReports = append(k.forwardReports, raw...)
					}
				}
			} else if !consumed {
				out = append(out, raw...)
			}
			return false
		}
		if event == 2 && k.consumed[code] {
			return false
		}
		if event == 1 {
			delete(k.consumed, code)
		}
		ctrl := mods & ^(64|128) == 4 // Lock-state bits do not change the binding.
		plain := mods & ^(64|128) == 0
		if code == 'c' && ctrl {
			k.Interrupted = true
		}
		if event == 2 {
			literal(raw)
			return false
		}
		consume := func() {
			if k.consumed == nil {
				k.consumed = make(map[int]bool)
			}
			k.consumed[code] = true
		}
		if code == 'b' && ctrl {
			consume()
			if len(k.prefix) > 0 {
				k.prefix = nil
				delete(k.consumed, code) // The second prefix is delivered to the application.
				out = append(out, k.forwardReports...)
				k.reports, k.forwardReports = nil, nil
				out = append(out, raw...)
			} else {
				k.prefix = append([]byte(nil), raw...)
			}
			return false
		}
		if len(k.prefix) == 0 {
			out = append(out, raw...)
			return false
		}
		var action KeyAction
		if plain {
			switch code {
			case 'c':
				action = KeyNew
			case 'n':
				action = KeyNext
			case 'p':
				action = KeyPrevious
			case 'l':
				action = KeyLast
			case 'd':
				action = KeyDetach
			case 'r':
				action = KeyRedraw
			}
		}
		if action == KeyNone {
			literal(raw)
			return false
		}
		consume()
		out = append(out, k.forwardReports...)
		k.reports, k.forwardReports = nil, nil
		k.prefix = nil
		flush()
		events = append(events, KeyEvent{Action: action})
		return action == KeyDetach || action.IsSessionCommand()
	}
	for _, c := range data {
		if k.mouseTail > 0 {
			out = append(out, c)
			k.mouseTail--
			continue
		}
		if k.opaque != 0 {
			out = append(out, c)
			if c == 0x18 || c == 0x1a || (c == 7 && k.opaque == ']') || (k.opaqueEscape && c == '\\') {
				k.opaque = 0
			}
			k.opaqueEscape = c == 0x1b
			continue
		}
		if k.paste {
			out = append(out, c)
			target := []byte("\x1b[201~")
			k.marker = append(k.marker, c)
			if !bytes.HasPrefix(target, k.marker) {
				k.marker = nil
				if c == 0x1b {
					k.marker = append(k.marker, c)
				}
			}
			if bytes.Equal(target, k.marker) {
				k.paste = false
				k.marker = nil
			}
			continue
		}
		if k.parser == nil {
			k.parser = new(ansi.Parser)
			k.parser.SetParamsSize(32)
			k.parser.SetDataSize(1)
		}
		if k.parser.State() == parser.GroundState && c != 0x1b {
			code, mods := int(c), 0
			if c >= 1 && c <= 26 {
				code = int('a' + c - 1)
				mods = 4
			}
			if key([]byte{c}, code, mods, 1) {
				return events
			}
			continue
		}
		// ESC cancels an unfinished CSI/Escape. Preserve the abandoned bytes.
		state := k.parser.State()
		if c == 0x1b && (state == parser.EscapeState || state == parser.CsiEntryState || state == parser.CsiParamState || state == parser.CsiIntermediateState) {
			literal(k.sequence)
			k.sequence = nil
			k.streaming = false
		}
		if state == parser.EscapeState && bytes.ContainsRune([]byte("]P^_X"), rune(c)) {
			literal(k.sequence)
			out = append(out, c)
			k.sequence = nil
			k.streaming = false
			k.opaque = c
			k.opaqueEscape = false
			k.parser.Reset()
			continue
		}
		k.parser.Advance(c)
		if k.streaming {
			out = append(out, c)
		} else {
			k.sequence = append(k.sequence, c)
		}
		if k.parser.State() != parser.GroundState {
			// Only Escape/CSI can be a supported key. Stream opaque control strings
			// and overlong sequences without retaining their payload.
			st := k.parser.State()
			candidate := st == parser.EscapeState || st == parser.CsiEntryState || st == parser.CsiParamState || st == parser.CsiIntermediateState
			if !k.streaming && (!candidate || len(k.sequence) > 128) {
				literal(k.sequence)
				k.sequence = nil
				k.streaming = true
			}
			continue
		}
		if k.streaming {
			k.streaming = false
			continue
		}
		raw := k.sequence
		k.sequence = nil
		if bytes.Equal(raw, []byte("\x1b[200~")) {
			literal(raw)
			k.paste = true
			continue
		}
		if bytes.Equal(raw, []byte("\x1b[M")) {
			k.mouseTail = 3
		}
		code, mods, event, ok := decodeKey(k.parser, raw)
		if !ok {
			literal(raw)
		} else if key(raw, code, mods, event) {
			return events
		}
	}
	flush()
	return events
}

// decodeKey adds only the keyboard semantics sshore needs to the ANSI parser.
// Unknown or malformed reports retain their original representation.
func decodeKey(p *ansi.Parser, raw []byte) (code, mods, event int, ok bool) {
	if len(raw) > 128 || !bytes.HasPrefix(raw, []byte("\x1b[")) {
		return
	}
	digits := 0
	for _, c := range raw[2 : len(raw)-1] {
		if c >= '0' && c <= '9' {
			digits++
			if digits > 7 {
				return
			}
		} else {
			digits = 0
		}
		if (c < '0' || c > '9') && c != ';' && c != ':' {
			return
		}
	}
	params := p.Params()
	var groups [][]int
	for i := 0; i < len(params); {
		var group []int
		for {
			value, more, _ := params.Param(i, -1)
			group = append(group, value)
			i++
			if !more || i >= len(params) {
				break
			}
		}
		groups = append(groups, group)
	}
	event = 1
	switch p.Command() {
	case 'u':
		if len(groups) < 1 || len(groups) > 3 || len(groups[0]) > 3 {
			return
		}
		code = groups[0][0]
		// Kitty can supply the ASCII key in the standard layout for shortcuts.
		if code > 127 && len(groups[0]) == 3 && groups[0][2] > 0 && groups[0][2] < 128 {
			code = groups[0][2]
		}
		mods = 1
		if len(groups) > 1 {
			if len(groups[1]) > 2 {
				return
			}
			mods = groups[1][0]
			if mods == -1 {
				mods = 1
			}
			if len(groups[1]) == 2 {
				event = groups[1][1]
			}
		}
	case '~':
		if len(groups) != 3 || len(groups[0]) != 1 || len(groups[1]) != 1 || len(groups[2]) != 1 || groups[0][0] != 27 {
			return
		}
		mods, code = groups[1][0], groups[2][0]
	default:
		return
	}
	if code < 0 || code > 0x10ffff || mods < 1 || mods > 256 || event < 1 || event > 3 {
		return
	}
	mods--
	ok = true
	return
}

func (a KeyAction) IsSessionCommand() bool { return a >= KeyNew && a <= KeyLast }

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

package tmux

// Track bounded 7-bit xterm input modes while detached or muted. Screen,
// scrollback, enhanced keyboard stacks and passthrough wrappers are not reconstructed.

import (
	"bytes"
	"strconv"
	"strings"
)

var mouseModes = [...]string{"9", "1000", "1001", "1002", "1003", "1005", "1006", "1015", "1016"}
var inputModes = [...]string{"9", "1000", "1001", "1002", "1003", "1005", "1006", "1015", "1016", "1004", "2004", "1", "2031", "2033", "2048"}

func MouseResetSequence() string {
	var b strings.Builder
	for _, mode := range mouseModes {
		b.WriteString("\x1b[?" + mode + "l")
	}
	return b.String()
}

// Observe, never filter, the PTY byte stream. Keep tracking while detached or
// muted so a later attachment restores the application's current mouse modes.
// Control strings are opaque: text inside OSC/DCS must not enable mouse input.
// The parser is bounded and accepts CSI sequences split across arbitrary reads.
type InputState struct {
	enabled               [len(inputModes)]bool
	saved                 [len(inputModes)]bool
	savedSet              [len(inputModes)]bool
	touched               [len(inputModes)]bool
	seen                  bool
	state                 byte
	csi                   []byte
	stringKind            byte
	keypad, keypadTouched bool
}

func (s *InputState) Observe(data []byte) {
	for len(data) > 0 {
		if s.state == 0 {
			// Ordinary output cannot change input modes; scan directly to the next ESC.
			i := bytes.IndexByte(data, 0x1b)
			if i < 0 {
				return
			}
			data = data[i:]
		}
		c := data[0]
		data = data[1:]
		if c == 0x18 || c == 0x1a {
			s.state = 0
			s.csi = s.csi[:0]
			continue
		}
		switch s.state {
		case 's', 't':
			if (c == 7 && s.stringKind == ']') || (s.state == 't' && c == '\\') {
				s.state = 0
			} else if c == 0x1b {
				s.state = 't'
			} else {
				s.state = 's'
			}
		case 'e':
			switch c {
			case '[':
				s.state = 'c'
				s.csi = s.csi[:0]
			case ']', 'P', '^', '_', 'X':
				s.state = 's'
				s.stringKind = c
			case 'c':
				s.enabled = [len(inputModes)]bool{}
				// RIS resets every tracked mode, including modes never explicitly
				// set by this application on the previous terminal attachment.
				for i := range s.touched {
					s.touched[i] = true
				}
				s.saved = [len(inputModes)]bool{}
				s.savedSet = [len(inputModes)]bool{}
				s.seen = true
				s.keypad = false
				s.keypadTouched = true
				s.state = 0
			case '=', '>':
				s.keypad = c == '='
				s.keypadTouched = true
				s.state = 0
			case 0x1b:
			default:
				s.state = 0
			}
		case 'c':
			if c == 0x1b {
				s.state = 'e'
				continue
			}
			if c < 0x20 || c == 0x7f {
				continue
			}
			if c >= 0x40 && c <= 0x7e {
				if c == 'h' || c == 'l' || c == 's' || c == 'r' {
					s.apply(c)
				}
				s.state = 0
			} else if len(s.csi) < 128 {
				s.csi = append(s.csi, c)
			} else {
				s.state = 'd'
			}
		case 'd':
			if c == 0x1b {
				s.state = 'e'
			} else if c >= 0x40 && c <= 0x7e {
				s.state = 0
			}
		default:
			if c == 0x1b {
				s.state = 'e'
			}
		}
	}
}

func (s *InputState) apply(action byte) {
	if len(s.csi) == 0 || s.csi[0] != '?' {
		return
	}
	params := strings.Split(string(s.csi[1:]), ";")
	for _, p := range params {
		if _, err := strconv.ParseUint(p, 10, 16); err != nil {
			return
		}
	}
	for _, p := range params {
		n, _ := strconv.Atoi(p)
		for i, mode := range inputModes {
			if strconv.Itoa(n) == mode {
				s.touched[i] = true
				if action == 's' {
					s.saved[i] = s.enabled[i]
					s.savedSet[i] = true
					s.seen = true
					continue
				}
				on := action == 'h'
				if action == 'r' {
					on = s.saved[i]
				}
				// X10, normal, highlight, button and any-motion tracking are exclusive.
				if on && i < 5 {
					for j := 0; j < 5; j++ {
						s.enabled[j] = false
					}
				}
				s.enabled[i] = on
				s.seen = true
			}
		}
	}
}

func (s *InputState) resetSequence() string {
	var b strings.Builder
	b.WriteString(MouseResetSequence())
	for i := len(mouseModes); i < len(inputModes); i++ {
		if s.touched[i] {
			b.WriteString("\x1b[?" + inputModes[i] + "l")
		}
	}
	return b.String()
}

func (s *InputState) Replay() []byte {
	var b strings.Builder
	if s.seen {
		b.WriteString(s.resetSequence())
	}
	// Recreate the application's saved values as well: a later private-mode restore
	// may arrive without another save on this new terminal attachment.
	saved := false
	for i, mode := range inputModes {
		if s.savedSet[i] {
			saved = true
			if s.saved[i] {
				b.WriteString("\x1b[?" + mode + "h")
			}
			b.WriteString("\x1b[?" + mode + "s")
			if s.saved[i] {
				b.WriteString("\x1b[?" + mode + "l")
			}
		}
	}
	if saved {
		b.WriteString(s.resetSequence())
	}
	for i, mode := range inputModes {
		if s.enabled[i] {
			b.WriteString("\x1b[?" + mode + "h")
		}
	}
	if s.keypadTouched {
		if s.keypad {
			b.WriteString("\x1b=")
		} else {
			b.WriteString("\x1b>")
		}
	}
	// Preserve an in-flight control prefix at the attachment boundary.
	if s.state == 'c' {
		b.WriteString("\x1b[")
		b.Write(s.csi)
	} else if s.state == 'e' {
		b.WriteByte(0x1b)
	} else if s.state == 's' || s.state == 't' {
		// Do not expose the remainder of an opaque string as terminal commands
		// when attaching between its opener and terminator. No payload is saved.
		b.WriteByte(0x1b)
		b.WriteByte(s.stringKind)
		if s.state == 't' {
			b.WriteByte(0x1b)
		}
	}
	return []byte(b.String())
}

// MouseEnabled reports whether native mouse capture is requested.
func (s *InputState) MouseEnabled() bool {
	for _, on := range s.enabled[:5] {
		if on {
			return true
		}
	}
	return false
}

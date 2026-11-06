package main

// Track standard 7-bit xterm mouse sequences even while output is detached or
// muted; reattachment restores only those modes. This is not a terminal emulator:
// screens, scrollback, focus/paste/keyboard state and terminal-specific passthrough
// wrappers are not reconstructed. Existing daemons keep their original behavior.

import (
	"strconv"
	"strings"
)

var mouseModes = [...]string{"9", "1000", "1001", "1002", "1003", "1005", "1006", "1015", "1016"}

func mouseResetSequence() string {
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
type terminalMouseState struct {
	enabled    [len(mouseModes)]bool
	saved      [len(mouseModes)]bool
	savedSet   [len(mouseModes)]bool
	seen       bool
	state      byte
	csi        []byte
	stringKind byte
}

func (s *terminalMouseState) observe(data []byte) {
	for _, c := range data {
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
				s.enabled = [len(mouseModes)]bool{}
				s.saved = [len(mouseModes)]bool{}
				s.savedSet = [len(mouseModes)]bool{}
				s.seen = true
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

func (s *terminalMouseState) apply(action byte) {
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
		for i, mode := range mouseModes {
			if strconv.Itoa(n) == mode {
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

func (s *terminalMouseState) replay() []byte {
	var b strings.Builder
	if s.seen {
		b.WriteString(mouseResetSequence())
	}
	// Recreate the application's saved values as well: a later private-mode restore
	// may arrive without another save on this new terminal attachment.
	saved := false
	for i, mode := range mouseModes {
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
		b.WriteString(mouseResetSequence())
	}
	for i, mode := range mouseModes {
		if s.enabled[i] {
			b.WriteString("\x1b[?" + mode + "h")
		}
	}
	// Preserve an in-flight CSI prefix at the attachment boundary.
	if s.state == 'c' {
		b.WriteString("\x1b[")
		b.Write(s.csi)
	} else if s.state == 'e' {
		b.WriteByte(0x1b)

	}
	return []byte(b.String())
}

package pty

// Track bounded 7-bit xterm input modes and alternate-screen selection while
// detached. Screen contents and scrollback are not reconstructed.

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
)

var inputModes = [...]int{9, 1000, 1001, 1002, 1003, 1005, 1006, 1015, 1016, 1004, 2004, 1, 2031, 2033, 2048}

const (
	mouseModeCount    = 9
	trackingModeCount = 5
)

type modeSetting uint8

const (
	modeUnset modeSetting = iota
	modeOff
	modeOn
)

func writeMode(b *strings.Builder, mode int, action byte) {
	b.WriteString("\x1b[?")
	b.WriteString(strconv.Itoa(mode))
	b.WriteByte(action)
}

func MouseResetSequence() string {
	var b strings.Builder
	for _, mode := range inputModes[:mouseModeCount] {
		writeMode(&b, mode, 'l')
	}
	return b.String()
}

const (
	inputText = iota
	inputEscape
	inputCSI
	inputDiscardCSI
	inputString
)

const escape = 0x1b

// Observe, never filter, the PTY byte stream. Track modes while detached and
// retain only the control prefix needed at the next attachment boundary.
type InputState struct {
	current, saved  [len(inputModes)]modeSetting
	prefix          []byte
	state           uint8
	keypad          modeSetting
	modifyOtherKeys string
	alternateScreen int // DEC mode that selected the alternate buffer, or zero
}

func (s *InputState) Observe(data []byte) {
	for len(data) > 0 {
		if s.state == inputText {
			i := bytes.IndexByte(data, escape)
			if i < 0 {
				return
			}
			s.prefix = append(s.prefix[:0], escape)
			s.state = inputEscape
			data = data[i+1:]
			continue
		}
		c := data[0]
		data = data[1:]
		if c == 0x18 || c == 0x1a {
			s.state = inputText
			continue
		}
		switch s.state {
		case inputString:
			if c == 7 && s.prefix[1] == ']' || c == '\\' && len(s.prefix) == 3 {
				s.state = inputText
			} else if c == escape {
				s.prefix = append(s.prefix[:2], escape)
			} else {
				s.prefix = s.prefix[:2]
			}
		case inputEscape:
			switch c {
			case '[':
				s.prefix = append(s.prefix, c)
				s.state = inputCSI
			case ']', 'P', '^', '_', 'X':
				s.prefix = append(s.prefix, c)
				s.state = inputString
			case escape:
				continue
			case 'c':
				// RIS explicitly resets every tracked mode.
				for i := range s.current {
					s.current[i] = modeOff
				}
				clear(s.saved[:])
				s.keypad = modeOff
				s.alternateScreen = 0
				s.modifyOtherKeys = ""
				s.state = inputText
			case '=', '>':
				s.keypad = modeOff
				if c == '=' {
					s.keypad = modeOn
				}
				s.state = inputText
			default:
				s.state = inputText
			}
		case inputCSI, inputDiscardCSI:
			switch {
			case c == escape:
				s.prefix = s.prefix[:1]
				s.state = inputEscape
			case c >= 0x40 && c <= 0x7e:
				if s.state == inputCSI && c == 'm' {
					switch string(s.prefix) {
					case "\x1b[>4", "\x1b[>4;0":
						s.modifyOtherKeys = ""
					case "\x1b[>4;1", "\x1b[>4;2":
						s.modifyOtherKeys = string(s.prefix) + "m"
					}
				}
				if s.state == inputCSI && (c == 'h' || c == 'l' || c == 's' || c == 'r') {
					s.apply(c)
				}
				s.state = inputText
			case s.state == inputCSI && c >= 0x20 && c != 0x7f:
				if len(s.prefix) == 2+128 {
					s.state = inputDiscardCSI
				} else {
					s.prefix = append(s.prefix, c)
				}
			}
		}
	}
}

func (s *InputState) apply(action byte) {
	if len(s.prefix) < 3 || s.prefix[2] != '?' {
		return
	}
	// Commit the whole CSI only after all parameters have validated.
	current, saved := s.current, s.saved
	alternateScreen := s.alternateScreen
	for param := range strings.SplitSeq(string(s.prefix[3:]), ";") {
		number, err := strconv.ParseUint(param, 10, 16)
		if err != nil {
			return
		}
		switch number {
		case 47, 1047, 1049:
			switch action {
			case 'h':
				alternateScreen = int(number)
			case 'l':
				alternateScreen = 0
			}
			continue
		}
		i := slices.Index(inputModes[:], int(number))
		if i < 0 {
			continue
		}
		setting := modeOff
		switch action {
		case 's':
			if current[i] == modeUnset {
				current[i] = modeOff
			}
			saved[i] = current[i]
			continue
		case 'r':
			if saved[i] == modeOn {
				setting = modeOn
			}
		case 'h':
			setting = modeOn
		}
		// X10, normal, highlight, button and any-motion tracking are exclusive.
		if setting == modeOn && i < trackingModeCount {
			for j := range trackingModeCount {
				if current[j] == modeOn {
					current[j] = modeOff
				}
			}
		}
		current[i] = setting
	}
	s.current, s.saved = current, saved
	s.alternateScreen = alternateScreen
}

func (s *InputState) Replay() []byte {
	var b strings.Builder
	if s.alternateScreen != 0 {
		writeMode(&b, s.alternateScreen, 'h')
	}
	if s.current != [len(inputModes)]modeSetting{} {
		for i, mode := range inputModes {
			if i < mouseModeCount || s.current[i] != modeUnset {
				writeMode(&b, mode, 'l')
			}
		}
	}
	// Recreate saved values and leave each mode off until restoring current modes.
	for i, mode := range inputModes {
		switch s.saved[i] {
		case modeOn:
			writeMode(&b, mode, 'h')
			writeMode(&b, mode, 's')
			writeMode(&b, mode, 'l')
		case modeOff:
			writeMode(&b, mode, 's')
		}
	}
	for i, mode := range inputModes {
		if s.current[i] == modeOn {
			writeMode(&b, mode, 'h')
		}
	}
	switch s.keypad {
	case modeOn:
		b.WriteString("\x1b=")
	case modeOff:
		b.WriteString("\x1b>")
	}
	switch s.state {
	case inputEscape, inputCSI, inputString:
		b.Write(s.prefix)
	}
	return []byte(b.String())
}

// MouseEnabled reports whether native mouse capture is requested.
func (s *InputState) MouseEnabled() bool {
	return slices.Contains(s.current[:trackingModeCount], modeOn)
}

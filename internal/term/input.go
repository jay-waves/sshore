package term

// Track bounded 7-bit xterm input modes and alternate-screen selection while
// detached. Screen contents and scrollback are not reconstructed.

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
)

var inputModes = [...]int{9, 1000, 1001, 1002, 1003, 1005, 1006, 1015, 1016, 1004, 2004, 1, 2031, 2033, 2048}

const trackingModeCount = 5

type savedMode uint8

const (
	notSaved savedMode = iota
	savedOff
	savedOn
)

// inputModesState is the application's input configuration. The zero value
// matches the terminal baseline used before applying a snapshot.
type inputModesState struct {
	enabled         [len(inputModes)]bool
	saved           [len(inputModes)]savedMode
	keypad          bool
	modifyOtherKeys string
	alternateScreen int
}

func writeMode(b *strings.Builder, mode int, action byte) {
	b.WriteString("\x1b[?")
	b.WriteString(strconv.Itoa(mode))
	b.WriteByte(action)
}

// InputState observes application output across arbitrary read boundaries.
type InputState struct {
	modes    inputModesState
	boundary controlBoundary
	params   []byte
}

func (s *InputState) Observe(data []byte) {
	s.observe(data, nil)
}

func (s *InputState) observe(data []byte, report *DirectoryReport) {
	for len(data) > 0 {
		if s.boundary.state == controlText {
			i := bytes.IndexByte(data, escape)
			if i < 0 {
				break
			}
			s.boundary.advance(escape)
			data = data[i+1:]
			continue
		}
		c := data[0]
		data = data[1:]
		previous := s.boundary.state
		next := s.boundary.advance(c)
		if report != nil && s.boundary.stringKind == ']' {
			report.advance(previous, next, c)
		}
		if c == 0x18 || c == 0x1a {
			continue
		}
		switch previous {
		case controlEscape:
			switch c {
			case '[':
				s.params = s.params[:0]
			case 'c':
				s.modes = inputModesState{}
			case '=', '>':
				s.modes.keypad = c == '='
			}
		case controlCSI:
			switch {
			case next == controlText:
				s.modes.applyCSI(string(s.params), c)
			case next == controlCSI && c >= 0x20 && c != 0x7f:
				if len(s.params) == 128 {
					s.boundary.state = controlDiscardCSI
				} else {
					s.params = append(s.params, c)
				}
			}
		}
	}
}

func (m *inputModesState) applyCSI(params string, action byte) {
	if action == 'm' {
		switch params {
		case ">4", ">4;0":
			m.modifyOtherKeys = ""
		case ">4;1", ">4;2":
			m.modifyOtherKeys = "\x1b[" + params + "m"
		}
		return
	}
	if !strings.HasPrefix(params, "?") || !strings.ContainsRune("hlsr", rune(action)) {
		return
	}

	// Apply a complete command atomically. An invalid parameter leaves all modes unchanged.
	next := *m
	for param := range strings.SplitSeq(params[1:], ";") {
		number, err := strconv.ParseUint(param, 10, 16)
		if err != nil {
			return
		}
		next.applyMode(int(number), action)
	}
	*m = next
}

func (m *inputModesState) applyMode(mode int, action byte) {
	switch mode {
	case 47, 1047, 1049:
		switch action {
		case 'h':
			m.alternateScreen = mode
		case 'l':
			m.alternateScreen = 0
		}
		return
	}

	i := slices.Index(inputModes[:], mode)
	if i < 0 {
		return
	}
	if action == 's' {
		m.saved[i] = savedOff
		if m.enabled[i] {
			m.saved[i] = savedOn
		}
		return
	}
	enabled := action == 'h' || action == 'r' && m.saved[i] == savedOn
	if enabled && i < trackingModeCount {
		clear(m.enabled[:trackingModeCount])
	}
	m.enabled[i] = enabled
}

// Replay restores the application configuration on top of a clean input
// baseline, then resumes any control sequence split by the attachment.
func (s *InputState) Replay() []byte {
	var b strings.Builder
	if s.modes.alternateScreen != 0 {
		writeMode(&b, s.modes.alternateScreen, 'h')
	}
	return s.appendReplay(&b)
}

func (s *InputState) appendReplay(b *strings.Builder) []byte {
	for i, mode := range inputModes {
		switch s.modes.saved[i] {
		case savedOn:
			writeMode(b, mode, 'h')
			writeMode(b, mode, 's')
			writeMode(b, mode, 'l')
		case savedOff:
			writeMode(b, mode, 's')
		}
	}
	for i, mode := range inputModes {
		if s.modes.enabled[i] {
			writeMode(b, mode, 'h')
		}
	}
	if s.modes.keypad {
		b.WriteString("\x1b=")
	}
	b.WriteString(s.modes.modifyOtherKeys)

	switch s.boundary.state {
	case controlEscape:
		b.WriteByte(escape)
	case controlCSI:
		b.WriteString("\x1b[")
		b.Write(s.params)
	case controlString, controlStringEscape:
		b.WriteByte(escape)
		b.WriteByte(s.boundary.stringKind)
		if s.boundary.state == controlStringEscape {
			b.WriteByte(escape)
		}
	}
	return []byte(b.String())
}

// MouseEnabled reports whether native mouse capture is requested.
func (s *InputState) MouseEnabled() bool {
	return slices.Contains(s.modes.enabled[:trackingModeCount], true)
}

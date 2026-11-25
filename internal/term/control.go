package term

const escape = 0x1b

type controlState uint8

const (
	controlText controlState = iota
	controlEscape
	controlCSI
	controlDiscardCSI
	controlString
	controlStringEscape
)

// controlBoundary recognizes 7-bit sequence boundaries without buffering bytes.
type controlBoundary struct {
	state      controlState
	stringKind byte
}

func (b *controlBoundary) advance(c byte) controlState {
	switch {
	case c == 0x18 || c == 0x1a:
		b.state = controlText
	case b.state == controlString || b.state == controlStringEscape:
		switch {
		case c == 7 && b.stringKind == ']' || c == '\\' && b.state == controlStringEscape:
			b.state = controlText
		case c == escape:
			b.state = controlStringEscape
		default:
			b.state = controlString
		}
	case c == escape:
		b.state = controlEscape
	case b.state == controlEscape:
		switch c {
		case '[':
			b.state = controlCSI
		case ']', 'P', '^', '_', 'X':
			b.state, b.stringKind = controlString, c
		default:
			b.state = controlText
		}
	case b.state == controlCSI || b.state == controlDiscardCSI:
		if c >= 0x40 && c <= 0x7e {
			b.state = controlText
		}
	}
	return b.state
}

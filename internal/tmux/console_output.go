package tmux

import (
	"io"
	"strings"

	"github.com/jay-waves/sshore/internal/transport"
)

// A nested ConPTY's Win32 input request must not change the client's VT reader
// into a native-event encoder. In VT-only hosts, those events are literal ESC,
// '[' and 'A' characters rather than an Up key. Leave every other mode intact.
type consoleOutput struct {
	out        io.Writer
	state      byte
	stringKind byte
	pending    []byte
}

func (w *consoleOutput) Write(data []byte) (int, error) {
	out := make([]byte, 0, len(data)+len(w.pending))
	for _, c := range data {
		switch w.state {
		case 's', 't':
			out = append(out, c)
			if (c == 7 && w.stringKind == ']') || (w.state == 't' && c == '\\') || c == 0x18 || c == 0x1a {
				w.state = 0
			} else if c == 0x1b {
				w.state = 't'
			} else {
				w.state = 's'
			}
		case 'e':
			if c == '[' {
				w.pending = append(w.pending, c)
				w.state = 'c'
			} else {
				out = append(out, w.pending...)
				w.pending = w.pending[:0]
				w.state = 0
				if c == 0x1b {
					w.pending = append(w.pending, c)
					w.state = 'e'
				} else {
					out = append(out, c)
					if c == ']' || c == 'P' || c == '^' || c == '_' || c == 'X' {
						w.state, w.stringKind = 's', c
					}
				}
			}
		case 'c':
			if c == 0x1b {
				out = append(out, w.pending...)
				w.pending = append(w.pending[:0], c)
				w.state = 'e'
				continue
			}
			w.pending = append(w.pending, c)
			if c >= 0x40 && c <= 0x7e {
				out = append(out, withoutWin32InputMode(w.pending)...)
				w.pending = w.pending[:0]
				w.state = 0
			} else if len(w.pending) >= 128 || c == 0x18 || c == 0x1a {
				out = append(out, w.pending...)
				w.pending = w.pending[:0]
				w.state = 0
			}
		default:
			if c == 0x1b {
				w.pending = append(w.pending, c)
				w.state = 'e'
			} else {
				out = append(out, c)
			}
		}
	}
	if err := transport.WriteFull(w.out, out); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (w *consoleOutput) Flush() error {
	err := transport.WriteFull(w.out, w.pending)
	w.pending = nil
	w.state = 0
	return err
}

func withoutWin32InputMode(seq []byte) []byte {
	if len(seq) < 5 || string(seq[:3]) != "\x1b[?" || (seq[len(seq)-1] != 'h' && seq[len(seq)-1] != 'l') {
		return seq
	}
	params := strings.Split(string(seq[3:len(seq)-1]), ";")
	kept := make([]string, 0, len(params))
	for _, p := range params {
		for _, c := range p {
			if c < '0' || c > '9' {
				return seq
			}
		}
		if strings.TrimLeft(p, "0") != "9001" {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(params) {
		return seq
	}
	if len(kept) == 0 {
		return nil
	}
	return []byte("\x1b[?" + strings.Join(kept, ";") + string(seq[len(seq)-1]))
}

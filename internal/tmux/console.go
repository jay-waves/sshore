package tmux

import (
	"io"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/jay-waves/sshore/internal/wire"
)

// At connection boundaries, clear reporting and restore prompt display modes
// before restoring local input settings. Unix sends cleanup only to a TTY whose
// TERM is not dumb; Windows temporarily enables VT and restores console flags.
// During connection, Windows output also excludes nested Win32 input requests.

// Mouse reporting belongs to the terminal emulator, not termios/console raw
// mode. A remote program can disable it while detached, when its output is
// discarded. Clear it at connection boundaries so mouse motion cannot become
// input to the local approval prompt or a resumed remote shell.
// Keep this a fixed boundary cleanup, not a screen/keyboard emulator.
const finishTerminalOutput = "\x18\x1b\\\x1b[?2026l"

func terminalInputResetSequence() string {
	// Cancel a truncated CSI/control string before sending cleanup. ST also
	// terminates strings on hosts which do not implement CAN inside OSC/DCS.
	// End synchronized output before returning to a prompt, even if the remote
	// application disconnected halfway through a frame.
	// Remote TUIs can leave Kitty release-event reporting or xterm
	// modifyOtherKeys enabled; termios/console restoration does not reset either.
	return finishTerminalOutput + "\x1b[=0u\x1b[>4;0m" + MouseResetSequence() +
		"\x1b[?1004l\x1b[?2004l\x1b[?1l\x1b[?2031l\x1b[?2033l\x1b[?2048l\x1b>\x1b]8;;\x1b\\\x1b[0m\x1b[?25h"
}

func ResetInputModes() {
	writeTerminalControl(os.Stdout, terminalInputResetSequence())
}

// RestoreScreen returns to the invoking terminal's main buffer if this
// attachment entered the alternate buffer.
func (s *InputState) RestoreScreen() {
	if s.alternateScreen == 0 {
		return
	}
	var sequence strings.Builder
	sequence.WriteString(finishTerminalOutput)
	writeMode(&sequence, s.alternateScreen, 'l')
	writeTerminalControl(os.Stdout, sequence.String())
}

// ReadConsole returns UTF-16. Decode it without os.File.Read's Windows Ctrl-Z
// EOF conversion, preserving job-control keys and split surrogate pairs.
type consoleInput struct {
	readUnits func([]uint16) (int, error)
	bytes     []byte
	high      uint16
	end       error
}

func (r *consoleInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.bytes) == 0 {
		if r.end != nil {
			return 0, r.end
		}
		var units [2048]uint16
		n, err := r.readUnits(units[:])
		if err != nil {
			r.end = err
		} else if n == 0 {
			r.end = io.EOF
		}
		for _, u := range units[:n] {
			if r.high != 0 {
				if u >= 0xdc00 && u <= 0xdfff {
					r.bytes = utf8.AppendRune(r.bytes, utf16.DecodeRune(rune(r.high), rune(u)))
					r.high = 0
					continue
				}
				r.bytes = utf8.AppendRune(r.bytes, utf8.RuneError)
				r.high = 0
			}
			if u >= 0xd800 && u <= 0xdbff {
				r.high = u
			} else if u >= 0xdc00 && u <= 0xdfff {
				r.bytes = utf8.AppendRune(r.bytes, utf8.RuneError)
			} else {
				r.bytes = utf8.AppendRune(r.bytes, rune(u))
			}
		}
		if r.end != nil && r.high != 0 {
			r.bytes = utf8.AppendRune(r.bytes, utf8.RuneError)
			r.high = 0
		}
	}
	n := copy(p, r.bytes)
	r.bytes = r.bytes[n:]
	return n, nil
}

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
	if err := wire.WriteFull(w.out, out); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (w *consoleOutput) Flush() error {
	err := wire.WriteFull(w.out, w.pending)
	w.pending = nil
	w.state = 0
	return err
}

func withoutWin32InputMode(seq []byte) []byte {
	original := seq
	var controls []byte
	// C0 controls execute inside CSI without becoming parameter bytes; DEL is
	// ignored. Keep them even when removing the surrounding mode request.
	for i := 2; i < len(seq); i++ {
		if seq[i] < 0x20 || seq[i] == 0x7f {
			clean := append([]byte(nil), seq[:i]...)
			for _, c := range seq[i:] {
				if c < 0x20 || c == 0x7f {
					controls = append(controls, c)
				} else {
					clean = append(clean, c)
				}
			}
			seq = clean
			break
		}
	}
	if len(seq) < 5 || string(seq[:3]) != "\x1b[?" || (seq[len(seq)-1] != 'h' && seq[len(seq)-1] != 'l') {
		return original
	}
	params := strings.Split(string(seq[3:len(seq)-1]), ";")
	kept := make([]string, 0, len(params))
	for _, p := range params {
		for _, c := range p {
			if c < '0' || c > '9' {
				return original
			}
		}
		if strings.TrimLeft(p, "0") != "9001" {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(params) {
		return original
	}
	if len(kept) == 0 {
		return controls
	}
	return append(controls, []byte("\x1b[?"+strings.Join(kept, ";")+string(seq[len(seq)-1]))...)
}

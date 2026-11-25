// Package term handles terminal byte streams and the local console.
package term

import (
	"io"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
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

// Both attachment and the command console restore snapshots from this baseline.
var inputModeBaseline = func() string {
	var b strings.Builder
	b.WriteString("\x1b[>4;0m\x1b>")
	for _, mode := range inputModes {
		writeMode(&b, mode, 'l')
	}
	return b.String()
}()

const resetPromptStyle = "\x1b]8;;\x1b\\\x1b[0m\x1b[?25h"

func terminalInputResetSequence() string {
	return finishTerminalOutput + "\x1b[=0u" + inputModeBaseline + resetPromptStyle
}

func ResetInputModes() {
	writeTerminalControl(os.Stdout, terminalInputResetSequence())
}

// BeginConsole prepares the screen and input modes for a temporary local
// prompt. Keep observing application output suspended until EndConsole returns.
func (s *InputState) BeginConsole(out io.Writer) error {
	sequence := finishTerminalOutput
	if s.modes.alternateScreen == 0 {
		sequence += "\x1b[?1049h"
	}
	sequence += "\x1b[>0u\x1b[=0u" + inputModeBaseline + resetPromptStyle + "\x1b[2J\x1b[H"
	return writeFull(out, []byte(sequence))
}

// EndConsole restores the application screen and input modes before buffered
// output is replayed. Applications on the alternate screen may need a redraw.
func (s *InputState) EndConsole(out io.Writer) error {
	var b strings.Builder
	b.WriteString(finishTerminalOutput)
	b.WriteString("\x1b[<1u")
	if s.modes.alternateScreen == 0 {
		b.WriteString("\x1b[?1049l")
	} else {
		b.WriteString("\x1b[2J\x1b[H")
	}
	b.WriteString(inputModeBaseline)
	return writeFull(out, s.appendReplay(&b))
}

// SetTitle requests an xterm-compatible window title and returns a restoration
// function. Terminals without title-stack support may ignore the push and pop.
func SetTitle(title string) func() {
	var safe strings.Builder
	for _, r := range title {
		if r >= 0x20 && r != 0x7f && !(r >= 0x80 && r <= 0x9f) {
			safe.WriteRune(r)
		}
	}
	writeTerminalControl(os.Stdout, "\x1b[22;0t\x1b]2;"+safe.String()+"\x1b\\")
	return func() { writeTerminalControl(os.Stdout, "\x1b[23;0t") }
}

// RestoreScreen returns to the invoking terminal's main buffer if this
// attachment entered the alternate buffer.
func (s *InputState) RestoreScreen() {
	if s.modes.alternateScreen == 0 {
		return
	}
	var sequence strings.Builder
	sequence.WriteString(finishTerminalOutput)
	writeMode(&sequence, s.modes.alternateScreen, 'l')
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
	out      io.Writer
	boundary controlBoundary
	pending  []byte
}

func (w *consoleOutput) Write(data []byte) (int, error) {
	out := make([]byte, 0, len(data)+len(w.pending))
	for _, c := range data {
		previous := w.boundary.state
		next := w.boundary.advance(c)
		switch {
		case previous == controlCSI && next == controlEscape:
			// Preserve cancellation even if the new escape sequence is filtered.
			out = append(out, w.pending...)
			out = append(out, 0x18)
			w.pending = append(w.pending[:0], c)
		case previous == controlCSI:
			w.pending = append(w.pending, c)
			if next == controlText {
				out = append(out, withoutWin32InputMode(w.pending)...)
				w.pending = w.pending[:0]
			} else if len(w.pending) >= 128 {
				out = append(out, w.pending...)
				w.pending = w.pending[:0]
				w.boundary.state = controlText
			}
		case next == controlCSI:
			w.pending = append(w.pending, c)
		default:
			out = append(out, w.pending...)
			w.pending = w.pending[:0]
			if next == controlEscape {
				w.pending = append(w.pending, c)
			} else {
				out = append(out, c)
			}
		}
	}
	if err := writeFull(w.out, out); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (w *consoleOutput) Flush() error {
	err := writeFull(w.out, w.pending)
	w.pending = nil
	w.boundary = controlBoundary{}
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

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

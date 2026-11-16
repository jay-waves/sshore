package termui

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"golang.org/x/term"
)

// Use ANSI palette indices rather than RGB values so Base16 and other terminal
// themes supply status colors. Only local UI uses this formatter;
// PTY bytes and protocol streams must never be decorated. Reset foreground color
// immediately, including after prompts, so typed paths do not inherit it.
// Respect NO_COLOR, redirected output and TERM=dumb. Progress/status use stderr;
// listings remain plain stdout. Sanitize control characters in displayed paths
// and errors while preserving readable UTF-8; never filter the remote PTY stream.

type Color string

const (
	Info    Color = "36"
	Success Color = "32"
	Warning Color = "33"
	Error   Color = "31"
)

func ColorEnabled(f *os.File) bool {
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(f.Fd())) && supportsANSI(f)
}

func Format(color bool, level Color, format string, args ...any) string {
	text := fmt.Sprintf(format, args...)
	if color {
		return "\x1b[" + string(level) + "m" + text + "\x1b[39m"
	}
	return text
}

func Log(f *os.File, level Color, format string, args ...any) {
	fmt.Fprint(f, Format(ColorEnabled(f), level, format, args...))
}

func CleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '\ufffd'
		}
		return r
	}, s)
}

package main

// Use ANSI palette indices rather than RGB values so Base16 and other terminal
// themes supply the colors. Only local UI messages pass through this formatter;
// PTY bytes and protocol streams must never be decorated. Reset foreground color
// immediately, including after prompts, so typed paths do not inherit it.
import (
	"fmt"
	"os"

	"golang.org/x/term"
)

type logColor string

const (
	logInfo    logColor = "36"
	logSuccess logColor = "32"
	logWarning logColor = "33"
	logError   logColor = "31"
)

func terminalLogColor(f *os.File) bool {
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(f.Fd())) && supportsANSI(f)
}

func formatLog(color bool, level logColor, format string, args ...any) string {
	text := fmt.Sprintf(format, args...)
	if color {
		return "\x1b[" + string(level) + "m" + text + "\x1b[39m"
	}
	return text
}

func logf(f *os.File, level logColor, format string, args ...any) {
	fmt.Fprint(f, formatLog(terminalLogColor(f), level, format, args...))
}

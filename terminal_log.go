package main

// Use ANSI palette indices rather than RGB values so Base16 and other terminal
// themes supply status colors. Bundled mascot artwork uses its own RGB palette
// after the same terminal capability check. Only local UI uses this formatter;
// PTY bytes and protocol streams must never be decorated. Reset foreground color
// immediately, including after prompts, so typed paths do not inherit it.
// Respect NO_COLOR, redirected output and TERM=dumb. Progress/status use stderr;
// listings remain plain stdout. Sanitize control characters in displayed paths
// and errors while preserving readable UTF-8; never filter the remote PTY stream.
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

package termui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Log writes a diagnostic or status line to a terminal or log file.
func Log(out io.Writer, id string, level Color, format string, args ...any) {
	color := false
	layout := "01-02 15:04"
	if f, ok := out.(*os.File); ok {
		color = ColorEnabled(f)
		if info, err := f.Stat(); err == nil && info.Mode().IsRegular() {
			layout = time.DateTime
		}
	}
	severity, label := Info, "info"
	if level == Error || level == Warning {
		severity, label = Error, "error"
	}
	stamp := Render(color, Muted, "["+time.Now().Format(layout)+"]")
	label = Render(color, severity, "["+label+"]")
	prefix := stamp + " " + label
	if id != "" {
		prefix += " " + Render(color, Accent, "["+id+"]")
	}
	message := strings.TrimSuffix(fmt.Sprintf(format, args...), "\n")
	message = strings.TrimPrefix(message, "[sshore] ")
	for line := range strings.SplitSeq(message, "\n") {
		fmt.Fprintf(out, "%s %s\n", prefix, line)
	}
}

const (
	Heading Color = "heading"
	Accent  Color = "accent"
	Muted   Color = "muted"
)

// Use ANSI palette colors so the terminal theme controls their appearance.
var styles = map[Color]ansi.Style{
	Heading: ansi.NewStyle().Bold(),
	Accent:  ansi.NewStyle().Bold().ForegroundColor(ansi.Cyan),
	Muted:   ansi.NewStyle().ForegroundColor(ansi.BrightBlack),
	Info:    ansi.NewStyle().ForegroundColor(ansi.Cyan),
	Success: ansi.NewStyle().ForegroundColor(ansi.Green),
	Warning: ansi.NewStyle().ForegroundColor(ansi.Yellow),
	Error:   ansi.NewStyle().ForegroundColor(ansi.Red),
}

// Format styles the text while preserving line endings used by a raw console.
func Format(color bool, level Color, format string, args ...any) string {
	text := fmt.Sprintf(format, args...)
	if !color {
		return text
	}
	var out strings.Builder
	for line := range strings.SplitAfterSeq(text, "\n") {
		body, hasLF := strings.CutSuffix(line, "\n")
		if hasLF {
			body = strings.TrimSuffix(body, "\r")
		}
		if body != "" {
			out.WriteString(Render(true, level, body))
		}
		out.WriteString(line[len(body):])
	}
	return out.String()
}

// StatusLine marks a concise status and leaves its text readable without color.
func StatusLine(color bool, level Color, text string) string {
	if !color {
		return text
	}
	marker := "•"
	switch level {
	case Success:
		marker = "✓"
	case Warning:
		marker = "!"
	case Error:
		marker = "×"
	}
	return Render(true, level, marker) + " " + Render(true, Heading, text)
}

// Render styles a piece of display text. Callers sanitize untrusted fields.
func Render(color bool, style Color, text string) string {
	if color && text != "" {
		return styles[style].Styled(text)
	}
	return text
}

// FormatPrompt resets the prompt style before the space where input begins.
func FormatPrompt(color bool, prompt string) string {
	label := strings.TrimRight(prompt, " ")
	return Render(color, Accent, label) + prompt[len(label):]
}

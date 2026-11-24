package termui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Log writes a diagnostic or status line to a terminal or log file.
func Log(out io.Writer, id string, level Color, format string, args ...any) {
	color := false
	if f, ok := out.(*os.File); ok {
		color = ColorEnabled(f)
	}
	if id == "" {
		id = "-"
	}
	severity, label := Info, "info"
	if level == Error || level == Warning {
		severity, label = Error, "error"
	}
	stamp := Render(color, Muted, "["+time.Now().Format("2006-01-02 15:04:05")+"]")
	identity := Render(color, Accent, "["+id+"]")
	label = Render(color, severity, "["+label+"]")
	message := strings.TrimSuffix(fmt.Sprintf(format, args...), "\n")
	message = strings.TrimPrefix(message, "[sshore] ")
	for line := range strings.SplitSeq(message, "\n") {
		fmt.Fprintf(out, "%s %s %s %s\n", stamp, identity, label, line)
	}
}

const (
	Heading Color = "heading"
	Accent  Color = "accent"
	Muted   Color = "muted"
)

// Use ANSI palette colors so the terminal theme controls their appearance.
var styles = map[Color]lipgloss.Style{
	Heading: lipgloss.NewStyle().Bold(true),
	Accent:  lipgloss.NewStyle().Bold(true).Foreground(ansi.Cyan),
	Muted:   lipgloss.NewStyle().Foreground(ansi.BrightBlack),
	Info:    lipgloss.NewStyle().Foreground(ansi.Cyan),
	Success: lipgloss.NewStyle().Foreground(ansi.Green),
	Warning: lipgloss.NewStyle().Foreground(ansi.Yellow),
	Error:   lipgloss.NewStyle().Foreground(ansi.Red),
}

// Format styles the text while preserving line endings used by a raw console.
func Format(color bool, level Color, format string, args ...any) string {
	text := fmt.Sprintf(format, args...)
	if !color {
		return text
	}
	style := styles[level].TabWidth(lipgloss.NoTabConversion)
	var out strings.Builder
	for line := range strings.SplitAfterSeq(text, "\n") {
		body, hasLF := strings.CutSuffix(line, "\n")
		if hasLF {
			body = strings.TrimSuffix(body, "\r")
		}
		if body != "" {
			out.WriteString(style.Render(body))
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
		return styles[style].Render(text)
	}
	return text
}

// FormatPrompt resets the prompt style before the space where input begins.
func FormatPrompt(color bool, prompt string) string {
	label := strings.TrimRight(prompt, " ")
	return Render(color, Accent, label) + prompt[len(label):]
}

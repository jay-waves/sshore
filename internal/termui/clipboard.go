package termui

import (
	"io"

	"github.com/charmbracelet/x/ansi"
)

func copyToClipboard(out io.Writer, value string) error {
	_, err := io.WriteString(out, ansi.SetSystemClipboard(value))
	return err
}

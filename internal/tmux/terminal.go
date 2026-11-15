// Package tmux manages client consoles, native terminals and exclusive attachments.
package tmux

import "fmt"

// Size is measured in character cells, excluding any client decorations.
type Size struct{ Cols, Rows uint16 }

func (s Size) validate() error {
	if s.Cols == 0 || s.Rows == 0 || s.Cols > 32767 || s.Rows > 32767 {
		return fmt.Errorf("terminal size must be between 1 and 32767")
	}
	return nil
}

// Config uses an executable path and separate arguments, never a shell command.
// A nil Env inherits the environment. Dir empty inherits the working directory.
type Config struct {
	Path      string
	Args, Env []string
	Dir       string
	Size      Size
}

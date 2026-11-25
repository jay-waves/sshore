// Package pty owns child processes and their native pseudo-terminals.
package pty

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

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
	Path       string
	Args, Env  []string
	Dir        string
	Size       Size
	TaskCgroup string // Linux delegated cgroup v2 parent.
}

func (c Config) getenv(key string) string {
	if c.Env == nil {
		return os.Getenv(key)
	}
	for _, entry := range slices.Backward(c.Env) {
		if len(entry) > len(key) && entry[len(key)] == '=' {
			name := entry[:len(key)]
			if name == key || runtime.GOOS == "windows" && strings.EqualFold(name, key) {
				return entry[len(key)+1:]
			}
		}
	}
	return ""
}

// Look up the shell using the launcher's environment and working directory.
// A bare name must not implicitly execute a file from a relative PATH entry.
func (c Config) executablePath() (string, error) {
	switch c.Path {
	case "", ".", "..":
		return "", &exec.Error{Name: c.Path, Err: exec.ErrNotFound}
	}
	if filepath.Base(c.Path) != c.Path {
		return c.findExecutable(c.Path)
	}
	for _, dir := range filepath.SplitList(c.getenv("PATH")) {
		if runtime.GOOS == "windows" && dir == "" {
			continue
		}
		path, err := c.findExecutable(filepath.Join(dir, c.Path))
		if err == nil {
			if !filepath.IsAbs(dir) {
				return "", &exec.Error{Name: c.Path, Err: exec.ErrDot}
			}
			return path, nil
		}
	}
	return "", &exec.Error{Name: c.Path, Err: exec.ErrNotFound}
}

// Package pty provides native shell terminals, console I/O and keyboard handling.
package pty

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
	Path      string
	Args, Env []string
	Dir       string
	Size      Size
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

type inputTerminal interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}

// InputLease prevents a former attachment's in-flight write from reaching a new
// owner. Stop must finish before another lease uses the same terminal.
type InputLease struct {
	terminal inputTerminal
	mu       sync.Mutex
	stopped  atomic.Bool
	once     sync.Once
	stopErr  error
}

func NewInputLease(t inputTerminal) *InputLease { return &InputLease{terminal: t} }
func (l *InputLease) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped.Load() {
		return 0, os.ErrClosed
	}
	return l.terminal.Write(p)
}
func (l *InputLease) Stop() error {
	l.once.Do(func() {
		l.stopped.Store(true)
		l.stopErr = l.terminal.SetWriteDeadline(time.Now())
		// A device without usable cancellation cannot safely transfer input ownership.
		if l.stopErr != nil {
			_ = l.terminal.Close()
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.stopErr == nil {
			l.stopErr = l.terminal.SetWriteDeadline(time.Time{})
		}
	})
	return l.stopErr
}

// Stopped distinguishes owner cancellation from an unexpected terminal failure.
func (l *InputLease) Stopped() bool { return l.stopped.Load() }

// StartupArgs selects startup mode; shells own startup-file processing.
func StartupArgs(path string, noLogin, noProfile bool) ([]string, error) {
	return startupArgs(runtime.GOOS, path, noLogin, noProfile)
}

func startupArgs(platform, path string, noLogin, noProfile bool) ([]string, error) {
	name := filepath.Base(path)
	if platform == "windows" {
		if noLogin {
			return nil, fmt.Errorf("--no-login requires a Unix shell; use --no-profile to skip Windows shell startup configuration")
		}
		if !noProfile {
			return nil, nil
		}
		name = strings.ToLower(filepath.Base(strings.ReplaceAll(path, "\\", "/")))
		switch name {
		case "pwsh", "pwsh.exe", "powershell", "powershell.exe":
			return []string{"-NoProfile"}, nil
		case "cmd", "cmd.exe":
			return []string{"/D"}, nil
		default:
			return nil, fmt.Errorf("--no-profile is unsupported for shell %q", path)
		}
	}
	if noProfile {
		switch name {
		case "bash":
			return []string{"--noprofile", "--norc", "-i"}, nil
		case "zsh":
			return []string{"-f", "-i"}, nil
		case "fish":
			return []string{"--no-config", "-i"}, nil
		case "sh":
			return []string{"-i"}, nil
		default:
			return nil, fmt.Errorf("--no-profile is unsupported for shell %q", path)
		}
	}
	if noLogin {
		return []string{"-i"}, nil
	}
	switch name {
	case "bash", "zsh", "fish", "sh", "dash", "ash", "ksh":
		return []string{"-l", "-i"}, nil
	default:
		return nil, fmt.Errorf("login startup is unsupported for shell %q; use --no-login for ordinary interactive startup", path)
	}
}

// StartupEnv preserves inherited variables except sh's optional startup hook.
func StartupEnv(path string, env []string, noProfile bool) []string {
	if runtime.GOOS != "linux" || !noProfile || filepath.Base(path) != "sh" {
		return env
	}
	filtered := make([]string, 0, len(env))
	for _, item := range env {
		if !strings.HasPrefix(item, "ENV=") {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

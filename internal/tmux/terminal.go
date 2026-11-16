// Package tmux manages client consoles, native terminals and exclusive attachments.
package tmux

import (
	"fmt"
	"io"
	"os"
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

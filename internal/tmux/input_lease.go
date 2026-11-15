package tmux

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

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

package tmux

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
)

// Attachment owns a terminal byte stream and its delivered input/output offsets.
type Attachment struct {
	terminal      TerminalStream
	done          chan struct{}
	closeOnce     sync.Once
	outputMu      sync.Mutex
	outputBytes   uint64
	outputErr     error
	inputBytes    atomic.Uint64
	inputProgress chan struct{}
	input         *inputLease
	inputDone     chan struct{}
	release       func() (Detachment, error)
}

// TerminalStream is an already connected terminal channel. A write deadline
// bounds delivery to a frontend that stops reading.
type TerminalStream interface {
	io.ReadWriteCloser
	SetWriteDeadline(time.Time) error
}

func NewAttachment(terminal TerminalStream) *Attachment {
	return &Attachment{terminal: terminal, done: make(chan struct{}), inputProgress: make(chan struct{}, 1)}
}

func (a *Attachment) Done() <-chan struct{} { return a.done }

func (a *Attachment) Close() {
	a.closeOnce.Do(func() {
		close(a.done)
		_ = a.terminal.Close()
	})
}

// Disconnect closes the channel and waits for the session to release input.
func (a *Attachment) Disconnect() (Detachment, error) {
	a.Close()
	if a.release == nil {
		return Detachment{}, nil
	}
	detached, err := a.release()
	<-a.inputDone
	return detached, err
}

func (a *Attachment) OutputStatus() (offset uint64, incomplete bool) {
	a.outputMu.Lock()
	defer a.outputMu.Unlock()
	return a.outputBytes, a.outputErr != nil
}

// DrainInput waits for bytes accepted by the PTY. A timeout leaves the barrier
// unacknowledged so the client can still detach and report uncertainty.
func (a *Attachment) DrainInput(offset uint64) (bool, error) {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for a.inputBytes.Load() < offset {
		select {
		case <-a.inputProgress:
		case <-a.Done():
			return false, io.ErrClosedPipe
		case <-timer.C:
			return false, nil
		}
	}
	return true, nil
}

func (a *Attachment) Output(data []byte) error {
	a.outputMu.Lock()
	defer a.outputMu.Unlock()
	if a.outputErr != nil {
		return a.outputErr
	}
	_ = a.terminal.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := writeFull(a.terminal, data)
	_ = a.terminal.SetWriteDeadline(time.Time{})
	if err != nil {
		a.outputErr = err
		_ = a.terminal.Close()
		return err
	}
	a.outputBytes += uint64(len(data))
	return nil
}

func (t *Session) startInput(a *Attachment) {
	a.inputDone = make(chan struct{})
	a.release = sync.OnceValues(func() (Detachment, error) { return t.Detach(a, nil) })
	input := a.input
	go func() {
		defer close(a.inputDone)
		a.readInput(input)
		select {
		case <-a.Done():
			// A dead terminal channel releases input even without a control loop.
			_, _ = a.release()
		default:
		}
	}()
}

func (a *Attachment) readInput(input *inputLease) {
	buf := make([]byte, 32*1024)
	for {
		n, err := a.terminal.Read(buf)
		if n > 0 {
			if e := writeFull(input, buf[:n]); e != nil {
				if !input.stopped.Load() {
					a.Close()
				}
				return
			}
			a.inputBytes.Add(uint64(n))
			select {
			case a.inputProgress <- struct{}{}:
			default:
			}
		}
		if err != nil {
			a.Close()
			return
		}
	}
}

type inputLease struct {
	terminal *pty.Terminal
	mu       sync.Mutex
	stopped  atomic.Bool
}

func (l *inputLease) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped.Load() {
		return 0, os.ErrClosed
	}
	return l.terminal.Write(p)
}
func (l *inputLease) Stop() error {
	l.stopped.Store(true)
	err := l.terminal.SetWriteDeadline(time.Now())
	// A device without usable cancellation cannot safely transfer input ownership.
	if err != nil {
		_ = l.terminal.Close()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		return err
	}
	return l.terminal.SetWriteDeadline(time.Time{})
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

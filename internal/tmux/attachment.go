package tmux

import (
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
)

// Attachment owns a terminal byte stream and its delivered input/output offsets.
// The caller owns the control protocol on the independent control connection.
type Attachment struct {
	control       io.Closer
	terminal      net.Conn
	done          chan struct{}
	closeOnce     sync.Once
	outputMu      sync.Mutex
	outputBytes   uint64
	outputErr     error
	inputBytes    atomic.Uint64
	inputProgress chan struct{}
	input         *inputLease
}

func NewAttachment(control io.Closer, terminal net.Conn) *Attachment {
	return &Attachment{control: control, terminal: terminal, done: make(chan struct{}), inputProgress: make(chan struct{}, 1)}
}

func (a *Attachment) stream() *Attachment { return a }

func (a *Attachment) Done() <-chan struct{} { return a.done }

func (a *Attachment) Close() {
	a.closeOnce.Do(func() {
		close(a.done)
		_ = a.terminal.Close()
		_ = a.control.Close()
	})
}

// AfterOutput reports the final offset in order with terminal writes.
func (a *Attachment) AfterOutput(report func(offset uint64, incomplete bool)) {
	a.outputMu.Lock()
	defer a.outputMu.Unlock()
	report(a.outputBytes, a.outputErr != nil)
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

func (a *Attachment) PumpInput() <-chan struct{} {
	done := make(chan struct{})
	input := a.input
	go func() {
		defer close(done)
		a.readInput(input)
	}()
	return done
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

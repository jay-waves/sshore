package tmux

import (
	"crypto/rand"
	"errors"
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

func NewAttachment(control io.Closer) *Attachment {
	return &Attachment{control: control, done: make(chan struct{}), inputProgress: make(chan struct{}, 1)}
}

func (a *Attachment) stream() *Attachment { return a }

func (a *Attachment) Done() <-chan struct{} { return a.done }

func (a *Attachment) Close() {
	a.closeOnce.Do(func() {
		close(a.done)
		if a.terminal != nil {
			_ = a.terminal.Close()
		}
		_ = a.control.Close()
	})
}

var ErrTerminalEndpoint = errors.New("terminal endpoint expired or already claimed")

type terminalEndpoint struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *terminalEndpoint) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		close(c.done)
	})
	return err
}

// OfferTerminal holds an authenticated byte stream for one attachment. The
// offer ends on timeout, session exit, or closure of the attached stream.
func (t *Task[A, T]) OfferTerminal(conn net.Conn, announce func(string) error) error {
	endpoint := &terminalEndpoint{Conn: conn, done: make(chan struct{})}
	defer endpoint.Close()
	token := rand.Text()
	t.guard.Lock()
	if t.Exited() || t.Stopping() {
		t.guard.Unlock()
		return ErrEnded
	}
	if len(t.pendingTerminals) >= 8 {
		t.guard.Unlock()
		return errors.New("too many pending terminal endpoints")
	}
	if t.pendingTerminals == nil {
		t.pendingTerminals = make(map[string]*terminalEndpoint)
	}
	t.pendingTerminals[token] = endpoint
	t.guard.Unlock()
	defer t.takeTerminal(token)
	timer := time.AfterFunc(5*time.Second, func() {
		if unused := t.takeTerminal(token); unused != nil {
			_ = unused.Close()
		}
	})
	defer timer.Stop()
	if err := announce(token); err != nil {
		return err
	}
	<-endpoint.done
	return nil
}

func (t *Task[A, T]) takeTerminal(token string) *terminalEndpoint {
	t.guard.Lock()
	defer t.guard.Unlock()
	endpoint := t.pendingTerminals[token]
	delete(t.pendingTerminals, token)
	return endpoint
}

func (t *Task[A, T]) closePendingTerminals() {
	t.guard.Lock()
	pending := t.pendingTerminals
	t.pendingTerminals = nil
	t.guard.Unlock()
	for _, endpoint := range pending {
		_ = endpoint.Close()
	}
}

// ServeAttachment pairs the streams, starts input after readiness, and drains
// the input lease when the frontend's control loop returns. The caller's serve
// function can report an admission error while the control stream is open.
func (t *Task[A, T]) ServeAttachment(owner A, req AttachRequest, ready func(string, Snapshot[A]) error, serve func(error) error) (result error) {
	a := owner.stream()
	var inputDone <-chan struct{}
	defer func() {
		a.Close()
		if inputDone == nil {
			return
		}
		detached, err := t.Detach(owner, nil)
		<-inputDone
		if t.events.AttachmentLost != nil && (err != nil || result != nil && detached.Released) {
			t.events.AttachmentLost(result, err)
		}
	}()
	terminal := t.takeTerminal(req.TerminalToken)
	if terminal == nil {
		return serve(ErrTerminalEndpoint)
	}
	a.terminal = terminal
	if req.ValidationError != nil {
		return serve(req.ValidationError)
	}
	if err := t.attach(owner, req, ready); err != nil {
		return serve(err)
	}
	inputDone = a.pumpInput()
	return serve(nil)
}

// FinishOutput reports the final offset and bounds the time to drain it.
func (a *Attachment) FinishOutput(report func(offset uint64, incomplete bool)) {
	a.outputMu.Lock()
	report(a.outputBytes, a.outputErr != nil)
	a.outputMu.Unlock()
	select {
	case <-a.Done():
	case <-time.After(2 * time.Second):
		a.Close()
	}
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

func (a *Attachment) pumpInput() <-chan struct{} {
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

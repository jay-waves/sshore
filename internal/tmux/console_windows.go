package tmux

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// Windows input uses ReadConsole directly; consoleInput decodes segmented UTF-16
// and preserves control keys such as Ctrl-Z. The manual Tests workflow runs
// native Windows race tests,
// but full interactive Windows Terminal behavior still needs manual testing;
// cross-compilation alone does not validate console interaction. Native mouse
// capture starts disabled and follows remote mouse requests; disabling Quick
// Edit alone must not enable mouse reports in a plain remote shell.

func RawConsole() (func(), error) {
	in, out := windows.Handle(os.Stdin.Fd()), windows.Handle(os.Stdout.Fd())
	var oldIn, oldOut uint32
	if err := windows.GetConsoleMode(in, &oldIn); err != nil {
		return nil, err
	}
	if err := windows.GetConsoleMode(out, &oldOut); err != nil {
		return nil, err
	}
	restore := func() {
		ResetInputModes()
		_ = windows.SetConsoleMode(in, oldIn)
		_ = windows.SetConsoleMode(out, oldOut)
	}
	// Do not inherit native mouse capture. With Quick Edit off, ConPTY
	// advertises ENABLE_MOUSE_INPUT to the outer terminal as mouse reporting,
	// even when the remote shell never requested it (with or without a mux).
	// Enter raw mode in one operation so no transient raw+mouse mode is exposed.
	mode := (oldIn | windows.ENABLE_VIRTUAL_TERMINAL_INPUT | windows.ENABLE_EXTENDED_FLAGS) &^
		(windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT |
			windows.ENABLE_QUICK_EDIT_MODE | windows.ENABLE_MOUSE_INPUT)
	err := windows.SetConsoleMode(in, mode)
	if err == nil {
		err = windows.SetConsoleMode(out, oldOut|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)
	}
	if err == nil {
		// Clear a mode inherited from an earlier nested console or client.
		_, err = os.Stdout.WriteString("\x1b[?9001l")
	}
	if err != nil {
		restore()
		return nil, fmt.Errorf("Windows VT-capable console required: %w", err)
	}
	return restore, nil
}

func terminalInputReader() io.Reader {
	return &consoleInput{readUnits: func(b []uint16) (int, error) {
		var n uint32
		err := windows.ReadConsole(windows.Handle(os.Stdin.Fd()), &b[0], uint32(len(b)), &n, nil)
		return int(n), err
	}}
}

func OutputWriter() (io.Writer, func() error) {
	w := &consoleOutput{out: os.Stdout}
	return w, w.Flush
}

// Approval runs before RawConsole enables VT output. Temporarily enable it
// for cleanup on Windows Terminal/ConPTY and VT-capable Console Host.
func writeTerminalControl(f *os.File, sequence string) {
	h := windows.Handle(f.Fd())
	var old uint32
	if windows.GetConsoleMode(h, &old) != nil {
		return
	}
	if windows.SetConsoleMode(h, old|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) != nil {
		return
	}
	defer windows.SetConsoleMode(h, old)
	_, _ = f.WriteString(sequence)
}

// ReadConsole consumes VT text, but ConPTY multiplexers also consult the native
// mouse flag. Keep that flag aligned with the remote application's requests.
func SyncMouse(state *InputState) error {
	in := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(in, &mode); err != nil {
		return err
	}
	desired := mode &^ windows.ENABLE_MOUSE_INPUT
	if state.MouseEnabled() {
		desired |= windows.ENABLE_MOUSE_INPUT
	}
	if desired == mode {
		return nil
	}
	return windows.SetConsoleMode(in, desired)
}

func FlushInput() error {
	return windows.FlushConsoleInputBuffer(windows.Handle(os.Stdin.Fd()))
}

var cancelTerminalSynchronousIO = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

// stop joins the pinned console reader before another session or SSH prompt can
// consume input. emit must not block after ctx is cancelled.
func StartKeyboard(parent context.Context, emit func([]byte, error)) (func(), error) {
	return startTerminalKeyboardReader(parent, emit, terminalInputReader())
}

func startTerminalKeyboardReader(parent context.Context, emit func([]byte, error), reader io.Reader) (func(), error) {
	if err := cancelTerminalSynchronousIO.Find(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	type startup struct {
		thread windows.Handle
		err    error
	}
	ready := make(chan startup)
	readDone := make(chan struct{})
	cancelDone := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
		ready <- startup{thread: thread, err: err}
		if err != nil {
			return
		}
		defer windows.CloseHandle(thread)
		// The cancellation worker must stop before this OS thread can be reused.
		defer func() { close(readDone); <-cancelDone }()
		buf := make([]byte, 4096)
		for ctx.Err() == nil {
			n, err := reader.Read(buf)
			if ctx.Err() != nil {
				return
			}
			if n > 0 {
				emit(append([]byte(nil), buf[:n]...), nil)
			}
			if err != nil {
				emit(nil, err)
				return
			}
		}
	}()
	started := <-ready
	if started.err != nil {
		cancel()
		<-done
		return nil, started.err
	}
	go func() {
		defer close(cancelDone)
		select {
		case <-readDone:
			return
		case <-ctx.Done():
		}
		// A single cancellation can miss the gap before ReadConsole starts. Retry on
		// a timer until the reader exits, keeping the thread pinned throughout.
		timer := time.NewTicker(10 * time.Millisecond)
		defer timer.Stop()
		for {
			cancelTerminalSynchronousIO.Call(uintptr(started.thread))
			select {
			case <-readDone:
				return
			case <-timer.C:
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}

var cancelSynchronousWrite = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

type writeRequest struct {
	data   []byte
	result chan writeResult
}
type writeResult struct {
	n   int
	err error
}
type threadInput struct {
	requests             chan writeRequest
	stop, done, released chan struct{}
	once                 sync.Once
	thread               windows.Handle
}

func NewTerminalInput(t *Terminal) (*threadInput, error) {
	if err := cancelSynchronousWrite.Find(); err != nil {
		return nil, err
	}
	l := &threadInput{requests: make(chan writeRequest), stop: make(chan struct{}), done: make(chan struct{}), released: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer func() { close(l.done); <-l.released }()
		var err error
		l.thread, err = windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
		ready <- err
		if err != nil {
			return
		}
		// Stop owns this handle until the cancellation loop has joined the writer.
		for {
			select {
			case <-l.stop:
				return
			case request := <-l.requests:
				select {
				case <-l.stop:
					request.result <- writeResult{err: os.ErrClosed}
					return
				default:
				}
				n, err := t.Write(request.data)
				request.result <- writeResult{n, err}
			}
		}
	}()
	if err := <-ready; err != nil {
		close(l.released)
		<-l.done
		return nil, err
	}
	return l, nil
}

func (l *threadInput) Write(p []byte) (int, error) {
	r := writeRequest{data: p, result: make(chan writeResult, 1)}
	select {
	case <-l.stop:
		return 0, os.ErrClosed
	case l.requests <- r:
	}
	result := <-r.result
	return result.n, result.err
}

func (l *threadInput) Stop() error {
	l.once.Do(func() {
		close(l.stop)
		defer close(l.released)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		defer windows.CloseHandle(l.thread)
		for {
			select {
			case <-l.done:
				return
			default:
			}
			cancelSynchronousWrite.Call(uintptr(l.thread))
			select {
			case <-l.done:
				return
			case <-ticker.C:
			}
		}
	})
	return nil
}

func (l *threadInput) Stopped() bool {
	select {
	case <-l.stop:
		return true
	default:
		return false
	}
}

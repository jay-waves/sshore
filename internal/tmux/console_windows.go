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

func terminalInputReader(handle windows.Handle) io.Reader {
	return &consoleInput{readUnits: func(b []uint16) (int, error) {
		var n uint32
		err := windows.ReadConsole(handle, &b[0], uint32(len(b)), &n, nil)
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

var cancelSynchronousIO = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

// stop joins the pinned console reader before another session or SSH prompt can
// consume input. emit must not block after ctx is cancelled.
func StartKeyboard(parent context.Context, emit func([]byte, error)) (func(), error) {
	// Give each reader its own console input handle. Reusing stdin after a
	// canceled ReadConsole can consume the next owner's first key on ConPTY.
	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	stop, err := startTerminalKeyboardReader(parent, emit, terminalInputReader(handle))
	if err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return sync.OnceFunc(func() { stop(); windows.CloseHandle(handle) }), nil
}

// startConsoleThread owns a synchronous I/O thread and its native handle.
func startConsoleThread(parent context.Context, run func(context.Context)) (func(), error) {
	if err := cancelSynchronousIO.Find(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	ready := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
		if err != nil {
			ready <- err
			return
		}
		defer windows.CloseHandle(thread)
		finished := make(chan struct{})
		interrupt := sync.OnceFunc(func() {
			// Cancellation can race with entry into the native call.
			timer := time.NewTicker(10 * time.Millisecond)
			defer timer.Stop()
			for {
				select {
				case <-finished:
					return
				default:
				}
				cancelSynchronousIO.Call(uintptr(thread))
				select {
				case <-finished:
					return
				case <-timer.C:
				}
			}
		})
		stopInterrupt := context.AfterFunc(ctx, interrupt)
		defer func() {
			// Join cancellation before releasing the handle and unpinning.
			close(finished)
			if !stopInterrupt() {
				interrupt()
			}
			cancel()
		}()
		ready <- nil
		run(ctx)
	}()
	if err := <-ready; err != nil {
		cancel()
		<-done
		return nil, err
	}
	return func() { cancel(); <-done }, nil
}

func startTerminalKeyboardReader(parent context.Context, emit func([]byte, error), reader io.Reader) (func(), error) {
	return startConsoleThread(parent, func(ctx context.Context) {
		buf := make([]byte, 4096)
		defer clear(buf)
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
	})
}

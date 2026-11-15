package tmux

import (
	"context"
	"io"
	"runtime"
	"time"

	"golang.org/x/sys/windows"
)

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

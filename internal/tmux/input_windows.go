package tmux

import (
	"os"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

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

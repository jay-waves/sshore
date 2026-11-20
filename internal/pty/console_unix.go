//go:build linux || (darwin && arm64)

package pty

import (
	"context"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// stop joins the reader without closing stdin. emit must not block after ctx is
// cancelled; each data slice belongs to the receiver.
func StartKeyboard(ctx context.Context, emit func([]byte, error)) (func(), error) {
	return startTerminalKeyboardFile(ctx, os.Stdin, emit)
}

func startTerminalKeyboardFile(parent context.Context, input *os.File, emit func([]byte, error)) (func(), error) {
	fd := int(input.Fd())
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		return nil, err
	}
	wake, wakeWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	// Closing the writer wakes Poll even when stdin is idle.
	wakeFD := int32(wake.Fd())
	signal := sync.OnceFunc(func() { _ = wakeWriter.Close() })
	stopWake := context.AfterFunc(ctx, signal)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			stopWake()
			signal()
			_ = wake.Close()
			cancel()
		}()
		buf := make([]byte, 4096)
		defer clear(buf)
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: wakeFD, Events: unix.POLLIN}}
		for ctx.Err() == nil {
			_, err := unix.Poll(poll, -1)
			if err == unix.EINTR {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				emit(nil, err)
				return
			}
			if poll[0].Revents&unix.POLLNVAL != 0 {
				emit(nil, os.ErrClosed)
				return
			}
			if poll[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) == 0 {
				continue
			}
			// This reader exclusively owns stdin between start and stop. Poll readiness
			// cannot be consumed by a new authentication reader until stop has joined it.
			if ctx.Err() != nil {
				return
			}
			n, err := input.Read(buf)
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
			if n == 0 {
				emit(nil, io.EOF)
				return
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}

func RawConsole() (func(), error) {
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() {
		ResetInputModes()
		_ = term.Restore(fd, state)
	}, nil
}

func terminalInputReader() io.Reader { return os.Stdin }

func OutputWriter() (io.Writer, func() error) {
	return os.Stdout, func() error { return nil }
}

func writeTerminalControl(f *os.File, sequence string) {
	if os.Getenv("TERM") == "dumb" || !term.IsTerminal(int(f.Fd())) {
		return
	}
	_, _ = f.WriteString(sequence)
}

func SyncMouse(state *InputState) error { return nil }

package ssh

import (
	"context"
	"io"
	"sync"
)

// OwnTransport binds a byte stream to its owner's cleanup and context. Closing
// it runs cleanup exactly once, including when cancellation races with Close.
// Cleanup replaces stream.Close: it must close the stream and release any owning
// connection or workers. A nil cleanup closes only the stream.
// This wrapper intentionally exposes no connection-specific methods.
func OwnTransport(ctx context.Context, stream io.ReadWriteCloser, cleanup func()) io.ReadWriteCloser {
	if cleanup == nil {
		cleanup = func() { _ = stream.Close() }
	}
	cleanup = sync.OnceFunc(cleanup)
	return &ownedTransport{
		ReadWriteCloser: stream,
		cleanup:         cleanup,
		stop:            context.AfterFunc(ctx, cleanup),
	}
}

type ownedTransport struct {
	io.ReadWriteCloser
	stop    func() bool
	cleanup func()
}

func (c *ownedTransport) Close() error {
	c.stop()
	c.cleanup()
	return nil
}

// Bridge relays opaque streams until either direction ends, then closes both
// endpoints and joins both copies. Used for attachment routes, where EOF ends
// the whole route. Close must unblock concurrent reads and writes.
func Bridge(a, b io.ReadWriteCloser) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(b, a)
		_ = b.Close()
		_ = a.Close()
		close(done)
	}()
	_, _ = io.Copy(a, b)
	_ = a.Close()
	_ = b.Close()
	<-done
}

// Splice relays a request/response connection, preserving half-close on clean
// EOF so a peer can send its response after receiving the complete request.
// Errors or cancellation abort both endpoints; return joins both copies and
// cancellation cleanup. Endpoints without CloseWrite fall back to full Close.
func Splice(ctx context.Context, a, b io.ReadWriteCloser) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { _ = a.Close(); _ = b.Close() }) }
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { closeBoth(); close(cancelled) })
	defer func() {
		closeBoth()
		if !stop() {
			<-cancelled
		}
	}()
	copyTo := func(dst, src io.ReadWriteCloser) {
		if _, err := io.Copy(dst, src); err != nil {
			closeBoth()
			return
		}
		if w, ok := dst.(interface{ CloseWrite() error }); ok {
			if w.CloseWrite() != nil {
				closeBoth()
			}
		} else {
			_ = dst.Close()
		}
	}
	done := make(chan struct{})
	go func() { copyTo(b, a); close(done) }()
	copyTo(a, b)
	<-done
}

package forward

import (
	"context"
	"io"
	"sync"
)

// Bridge relays opaque streams until either direction ends, then closes both
// endpoints and joins both copies. Used for attachment routes, where EOF ends
// the whole route. Close must unblock concurrent reads and writes.
func Bridge(a, b io.ReadWriteCloser) {
	relay(context.Background(), a, b, false)
}

// Splice relays a request/response connection, preserving half-close on clean
// EOF so a peer can send its response after receiving the complete request.
// Errors or cancellation abort both endpoints; return joins both copies and
// cancellation cleanup. Endpoints without CloseWrite fall back to full Close.
func Splice(ctx context.Context, a, b io.ReadWriteCloser) {
	relay(ctx, a, b, true)
}

func relay(ctx context.Context, a, b io.ReadWriteCloser, halfClose bool) {
	closeA := sync.OnceFunc(func() { _ = a.Close() })
	closeB := sync.OnceFunc(func() { _ = b.Close() })
	closeBoth := sync.OnceFunc(func() {
		// A protocol close can block on its underlying transport. Start
		// closing the other endpoint without waiting for it.
		done := make(chan struct{})
		go func() {
			defer close(done)
			closeB()
		}()
		closeA()
		<-done
	})
	defer closeBoth()
	if ctx.Done() != nil {
		stop := context.AfterFunc(ctx, closeBoth)
		defer stop()
	}
	copyTo := func(dst, src io.ReadWriteCloser, closeDst func()) {
		if _, err := io.Copy(dst, src); err != nil || !halfClose {
			closeBoth()
			return
		}
		if w, ok := dst.(interface{ CloseWrite() error }); ok {
			if w.CloseWrite() != nil {
				closeBoth()
			}
		} else {
			closeDst()
		}
	}
	done := make(chan struct{})
	go func() { copyTo(b, a, closeB); close(done) }()
	copyTo(a, b, closeA)
	<-done
}

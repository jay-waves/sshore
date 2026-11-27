package ports

import (
	"context"
	"io"
	"net"
	"sync"

	"github.com/xtaci/smux"
)

// relayTCP preserves half-close so a peer can reply after reading a full request.
func relayTCP(ctx context.Context, a, b io.ReadWriteCloser) {
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
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	copyTo := func(dst, src io.ReadWriteCloser, closeDst func()) {
		if _, err := io.Copy(dst, src); err != nil {
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

func serveTCPListener(ctx context.Context, tcp net.Listener, mux *smux.Session, workers *sync.WaitGroup) {
	for {
		conn, err := tcp.Accept()
		if err != nil {
			break
		}
		workers.Go(func() {
			stream, err := mux.OpenStream()
			if err != nil {
				conn.Close()
				return
			}
			relayTCP(ctx, conn, stream)
		})
	}
}

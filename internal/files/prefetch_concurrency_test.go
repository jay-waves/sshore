package files

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

type prefetchListingGate struct {
	entered chan string
	release chan struct{}
}

func (g *prefetchListingGate) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	<-g.release
	return emptyPrefetchListing{}, nil
}

type emptyPrefetchListing struct{}

func (emptyPrefetchListing) ListAt([]os.FileInfo, int64) (int, error) { return 0, io.EOF }

func TestDirectoryPrefetchOverlapsNetworkWaits(t *testing.T) {
	a, b := net.Pipe()
	gate := &prefetchListingGate{entered: make(chan string, 4), release: make(chan struct{})}
	server := sftp.NewRequestServer(a, sftp.Handlers{FileList: gate})
	go func() { _ = server.Serve() }()
	client, err := sftp.NewClientPipe(b, &prefetchRequestRecorder{WriteCloser: b, started: gate.entered})
	if err != nil {
		t.Fatal(err)
	}
	fs := &ftpFS{client: client, root: "/"}
	p := startDirectoryPrefetch(context.Background(), fs)
	defer func() {
		p.cancel()
		close(gate.release)
		b.Close()
		client.Close()
		server.Close()
		<-p.done
	}()
	p.mu.Lock()
	p.batch, p.cancelBatch = context.WithCancel(p.ctx)
	p.paths = []string{"/one", "/two", "/three"}
	p.mu.Unlock()
	for i := 0; i < directoryPrefetchWorkers; i++ {
		p.wake <- struct{}{}
	}
	for i := 0; i < directoryPrefetchWorkers; i++ {
		select {
		case <-gate.entered:
		case <-time.After(time.Second):
			t.Fatal("prefetch serialized independent network waits")
		}
	}
	select {
	case path := <-gate.entered:
		t.Fatal("concurrency exceeded limit", path)
	default:
	}
}

type prefetchRequestRecorder struct {
	io.WriteCloser
	started chan string
}

func (w *prefetchRequestRecorder) Write(b []byte) (int, error) {
	if len(b) >= 5 && b[4] == 11 {
		w.started <- "OPENDIR"
	}
	return w.WriteCloser.Write(b)
}

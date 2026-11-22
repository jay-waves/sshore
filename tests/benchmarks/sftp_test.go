package benchmarks

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/files"
)

// Fixed response latency preserves pipelining: sleeping in each Read/Write would
// instead serialize packets and measure an artificial bandwidth limit.
type latencyPacket struct {
	ready time.Time
	data  []byte
}
type latencyConn struct {
	net.Conn
	delay      time.Duration
	queue      chan latencyPacket
	stop, done chan struct{}
	once       sync.Once
}

func newLatencyConn(conn net.Conn, delay time.Duration) *latencyConn {
	c := &latencyConn{Conn: conn, delay: delay, queue: make(chan latencyPacket, 128), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			select {
			case <-c.stop:
				return
			case p := <-c.queue:
				timer := time.NewTimer(time.Until(p.ready))
				select {
				case <-c.stop:
					timer.Stop()
					return
				case <-timer.C:
				}
				if err := writeFull(c.Conn, p.data); err != nil {
					c.shutdown()
					return
				}
			}
		}
	}()
	return c
}
func (c *latencyConn) Write(p []byte) (int, error) {
	packet := latencyPacket{time.Now().Add(c.delay), bytes.Clone(p)}
	select {
	case <-c.stop:
		return 0, net.ErrClosed
	case c.queue <- packet:
		return len(p), nil
	}
}
func (c *latencyConn) shutdown()    { c.once.Do(func() { close(c.stop); c.Conn.Close() }) }
func (c *latencyConn) Close() error { c.shutdown(); <-c.done; return nil }

func BenchmarkSFTPCopyLatency(b *testing.B) {
	for _, delay := range []time.Duration{20 * time.Millisecond, 80 * time.Millisecond} {
		for _, operation := range []string{"sequential-download", "download", "upload-verified"} {
			b.Run(delay.String()+"/"+operation, func(b *testing.B) {
				root := b.TempDir()
				data := bytes.Repeat([]byte("12345678"), 128*1024)
				name := filepath.ToSlash(filepath.Join(root, "source"))
				if err := os.WriteFile(name, data, 0600); err != nil {
					b.Fatal(err)
				}
				a, z := net.Pipe()
				conn := newLatencyConn(a, delay)
				go func() { files.ServeCopy(conn); conn.Close() }()
				client, err := files.NewCopyClient(b.Context(), z)
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { client.Close(); conn.Close() })
				b.SetBytes(int64(len(data)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					switch operation {
					case "sequential-download":
						file, e := client.Open(name)
						if e != nil {
							b.Fatal(e)
						}
						_, e = file.Stat()
						if e == nil {
							e = files.CopyExactSize(sha256.New(), progressReader{Reader: file}, int64(len(data)))
						}
						file.Close()
						err = e
					case "download":
						err = files.DownloadTo(client, name, sha256.New(), int64(len(data)), nil)
					case "upload-verified":
						err = files.UploadReader(client, bytes.NewReader(data), filepath.ToSlash(filepath.Join(root, fmt.Sprint(i))), int64(len(data)))
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// progressReader preserves the sequential baseline's read path without progress.
type progressReader struct{ io.Reader }

func (r progressReader) Read(p []byte) (int, error) { return r.Reader.Read(p) }

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
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

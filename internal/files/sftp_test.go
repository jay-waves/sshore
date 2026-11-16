package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/transport"
	"github.com/pkg/sftp"
)

func TestSFTPRequestTimeoutIgnoresHealthySibling(t *testing.T) {
	wire := newFTPWire(strings.NewReader(""), nopWriteCloser{io.Discard})
	packet := make([]byte, 5)
	packet[0] = 3
	binary.BigEndian.PutUint32(packet[1:], 1)
	if e := wire.record(packet); e != nil {
		t.Fatal(e)
	}
	fired := make(chan struct{})
	watch := &sftpWatch{}
	stop := wire.watchRequests(30*time.Millisecond, watch, func() { close(fired) })
	defer stop()
	// Keep a second request/reply moving; it must not reset the stuck request's age.
	for i := uint32(2); i < 6; i++ {
		binary.BigEndian.PutUint32(packet[1:], i)
		if e := wire.record(packet); e != nil {
			t.Fatal(e)
		}
		packet[0] = 101
		if _, e := wire.filter(packet); e != nil {
			t.Fatal(e)
		}
		packet[0] = 3
	}
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("request failed to time out")
	}
	if watch.err() == nil || !strings.Contains(watch.err().Error(), "no retry") {
		t.Fatal(watch.err())
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
func TestSFTPIdleDoesNotTimeout(t *testing.T) {
	wire := newFTPWire(strings.NewReader(""), nopWriteCloser{io.Discard})
	watch := &sftpWatch{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := wire.watchRequests(10*time.Millisecond, watch, cancel)
	defer stop()
	select {
	case <-ctx.Done():
		t.Fatal("idle connection aborted")
	case <-time.After(35 * time.Millisecond):
	}
}

func TestSFTPTimeoutUnblocksPendingCall(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		init := make([]byte, 9)
		if _, e := io.ReadFull(a, init); e != nil {
			return
		}
		a.Write([]byte{0, 0, 0, 5, 2, 0, 0, 0, 3}) // VERSION 3
		io.Copy(io.Discard, a)                     // Consume requests, never respond.
	}()
	wire := newFTPWire(b, b)
	watch := &sftpWatch{}
	stop := wire.watchRequests(30*time.Millisecond, watch, func() { b.Close() })
	defer stop()
	client, e := sftp.NewClientPipe(wire, wire)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	done := make(chan error, 1)
	go func() { _, e := client.RealPath("."); done <- e }()
	select {
	case e := <-done:
		if e == nil || watch.err() == nil {
			t.Fatal(e, watch.err())
		}
	case <-time.After(time.Second):
		t.Fatal("pending SFTP call leaked")
	}
}

// Local in-process baseline only; not a WAN or FUSE benchmark. Compare request
// concurrency without changing production defaults or creating extra runtimes.

func BenchmarkSFTPReadConcurrency(b *testing.B) {
	for _, concurrency := range []int{8, 64} {
		b.Run(fmt.Sprint(concurrency), func(b *testing.B) {
			root := b.TempDir()
			path := filepath.Join(root, "file")
			data := make([]byte, 1024*1024)
			if e := os.WriteFile(path, data, 0600); e != nil {
				b.Fatal(e)
			}
			left, right := net.Pipe()
			server, e := sftp.NewServer(left)
			if e != nil {
				b.Fatal(e)
			}
			go func() { server.Serve(); server.Close() }()
			defer server.Close()
			wire := newFTPWire(right, right)
			client, e := sftp.NewClientPipe(wire, wire, sftp.MaxConcurrentRequestsPerFile(concurrency))
			if e != nil {
				b.Fatal(e)
			}
			defer client.Close()
			file, e := client.Open(filepath.ToSlash(path))
			if e != nil {
				b.Fatal(e)
			}
			defer file.Close()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, e := file.ReadAt(data, 0); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}

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
				if err := transport.WriteFull(c.Conn, p.data); err != nil {
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
				go func() { ServeCopy(conn); conn.Close() }()
				client, err := NewSFTP(z, z)
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
							e = CopyExactSize(sha256.New(), progressReader{Reader: file}, int64(len(data)))
						}
						file.Close()
						err = e
					case "download":
						err = DownloadTo(client, name, sha256.New(), int64(len(data)), nil)
					case "upload-verified":
						err = UploadReader(client, bytes.NewReader(data), filepath.ToSlash(filepath.Join(root, fmt.Sprint(i))), int64(len(data)))
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

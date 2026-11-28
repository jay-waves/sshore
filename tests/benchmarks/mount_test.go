//go:build linux || windows

package benchmarks

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	sftp "github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
)

func BenchmarkMountWireDataFrame(b *testing.B) {
	const id = 1
	data := make([]byte, files.MountSFTPDataLength)
	for _, upload := range []bool{false, true} {
		name := "read"
		var request sshfx.PacketMarshaller = &sshfx.ReadPacket{Handle: "bulk", Length: uint32(len(data))}
		var reply sshfx.PacketMarshaller = &sshfx.DataPacket{Data: data}
		if upload {
			name = "write"
			request = &sshfx.WritePacket{Handle: "bulk", Data: data}
			reply = &sshfx.StatusPacket{}
		}
		b.Run(name, func(b *testing.B) {
			requestHeader, requestBody, err := request.MarshalPacket(id, nil)
			if err != nil {
				b.Fatal(err)
			}
			replyHeader, replyBody, err := reply.MarshalPacket(id, nil)
			if err != nil {
				b.Fatal(err)
			}
			frame := append(replyHeader, replyBody...)
			buffer := make([]byte, binary.BigEndian.Uint32(frame[:4]))
			var reader bytes.Reader
			wire := files.NewSFTPWire(benchmarkWireStream{Reader: &reader, Writer: io.Discard})
			b.ReportAllocs()
			for b.Loop() {
				reader.Reset(frame)
				if err := wire.WriteSFTPPacket(requestHeader, requestBody); err != nil {
					b.Fatal(err)
				}
				var received sshfx.RawPacket
				if err := received.ReadFrom(wire, buffer, uint32(len(buffer))); err != nil {
					b.Fatal(err)
				}
				if received.RequestID != id || received.PacketType != sshfx.PacketType(frame[4]) {
					b.Fatal("unexpected reply")
				}
			}
		})
	}
}

type benchmarkWireStream struct {
	io.Reader
	io.Writer
}

func (benchmarkWireStream) Close() error { return nil }

func BenchmarkMountResponsiveness(b *testing.B) {
	for _, scheduled := range []bool{false, true} {
		mode := "shared-window"
		if scheduled {
			mode = "reserved-windows"
		}
		for _, operation := range []string{"readdir", "open-small-read"} {
			b.Run(mode+"/"+operation, func(b *testing.B) {
				benchmarkMountDuringDownload(b, scheduled, operation)
			})
		}
	}
}

func benchmarkMountDuringDownload(b *testing.B, scheduled bool, operation string) {
	root := filepath.ToSlash(b.TempDir())
	const block = 2 * files.MountSFTPDataLength
	if err := os.WriteFile(root+"/bulk", make([]byte, 32*block), 0600); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(root+"/small", []byte("short document"), 0600); err != nil {
		b.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	conn, err := (&net.Dialer{}).DialContext(b.Context(), "tcp", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	peer, err := listener.Accept()
	if err != nil {
		b.Fatal(err)
	}
	defer peer.Close()
	firstData := make(chan struct{})
	go func() {
		defer peer.Close()
		_ = files.ServeAgent(&pacedMountReplies{Conn: peer, firstData: sync.OnceFunc(func() { close(firstData) })},
			"mount_data", "0.0.0", "benchmark", "", nil, nil)
	}()
	// The agent waits for the client's INIT after sending its readiness line.
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		b.Fatal(err)
	}
	wire := files.NewSFTPWire(conn)
	var client *sftp.Client
	if scheduled {
		client, err = files.NewMountSFTPClient(b.Context(), wire, 0)
	} else {
		client, err = sftp.NewClientPipe(b.Context(), wire, wire, sftp.WithMaxDataLength(files.MountSFTPDataLength))
	}
	if err != nil {
		b.Fatal(err)
	}
	defer client.Close()
	bulk, err := client.Open(root + "/bulk")
	if err != nil {
		b.Fatal(err)
	}
	defer bulk.Close()

	var readers sync.WaitGroup
	var transferred atomic.Uint64
	stop := make(chan struct{})
	defer func() { close(stop); readers.Wait() }()
	for worker := range 32 {
		readers.Go(func() {
			buf := make([]byte, block)
			for {
				select {
				case <-stop:
					return
				default:
				}
				n, err := bulk.ReadAt(buf, int64(worker*block))
				if err != nil {
					b.Error(err)
					return
				}
				transferred.Add(uint64(n))
			}
		})
	}
	select {
	case <-firstData:
	case <-time.After(5 * time.Second):
		b.Fatal("bulk reads did not reach the server")
	}

	latencies := make([]time.Duration, 0, b.N)
	startBytes := transferred.Load()
	b.ResetTimer()
	start := time.Now()
	for range b.N {
		started := time.Now()
		if operation == "readdir" {
			if entries, err := client.ReadDir(root); err != nil || len(entries) != 2 {
				b.Fatalf("read directory: %d entries, %v", len(entries), err)
			}
		} else {
			small, err := client.Open(root + "/small")
			if err != nil {
				b.Fatal(err)
			}
			var buf [14]byte
			_, readErr := io.ReadFull(small, buf[:])
			closeErr := small.Close()
			if readErr != nil || closeErr != nil || string(buf[:]) != "short document" {
				b.Fatalf("read small file: %q, %v, %v", buf, readErr, closeErr)
			}
		}
		latencies = append(latencies, time.Since(started))
	}
	elapsed := time.Since(start)
	b.StopTimer()
	slices.Sort(latencies)
	p95 := (95*len(latencies)+99)/100 - 1
	b.ReportMetric(float64(latencies[p95])/float64(time.Millisecond), "p95-ms")
	b.ReportMetric(float64(transferred.Load()-startBytes)/elapsed.Seconds()/(1024*1024), "bulk-MiB/s")
}

// The local TCP buffers let the client pipeline requests. Pacing large
// replies models competition for a shared bottleneck without slowing metadata.
type pacedMountReplies struct {
	net.Conn
	firstData func()
}

func (c *pacedMountReplies) Write(p []byte) (int, error) {
	if len(p) == files.MountSFTPDataLength {
		c.firstData()
		time.Sleep(2 * time.Millisecond)
	}
	return c.Conn.Write(p)
}

//go:build linux || windows

package benchmarks

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/ssh"
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
		for _, operation := range []string{"readdir", "open-small-read", "watch-ack"} {
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
	carrier := &pacedMountReplies{Conn: peer, firstData: sync.OnceFunc(func() { close(firstData) })}
	go func() {
		defer peer.Close()
		_ = files.ServeMount(carrier, "0.0.0", "benchmark", "", nil, nil)
	}()
	mux, err := ssh.NewStreamMux(conn, true)
	if err != nil {
		b.Fatal(err)
	}
	defer mux.Close()
	data, _, err := files.OpenMountData(mux, "0.0.0", nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	watch, err := mux.OpenStream()
	if err != nil {
		b.Fatal(err)
	}
	defer watch.Close()
	watchRequest := struct {
		Type      string `json:"type"`
		Directory string `json:"directory"`
	}{Type: "watch", Directory: root}
	if !strings.HasPrefix(root, "/") {
		watchRequest.Directory = "/" + root
	}
	watchEncoder, watchDecoder := json.NewEncoder(watch), json.NewDecoder(watch)
	wire := files.NewSFTPWire(data)
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
				_, err := bulk.ReadAt(buf, int64(worker*block))
				if err != nil {
					b.Error(err)
					return
				}
			}
		})
	}
	select {
	case <-firstData:
	case <-time.After(5 * time.Second):
		b.Fatal("bulk reads did not reach the server")
	}

	latencies := make([]time.Duration, 0, b.N)
	startBytes := carrier.bulkWritten.Load()
	b.ResetTimer()
	start := time.Now()
	for range b.N {
		started := time.Now()
		switch operation {
		case "readdir":
			if entries, err := client.ReadDir(root); err != nil || len(entries) != 2 {
				b.Fatalf("read directory: %d entries, %v", len(entries), err)
			}
		case "open-small-read":
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
		case "watch-ack":
			var ack struct {
				Type  string `json:"type"`
				Error string `json:"error"`
			}
			if err := watch.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				b.Fatal(err)
			}
			if err := watchEncoder.Encode(watchRequest); err != nil {
				b.Fatal(err)
			}
			if err := watchDecoder.Decode(&ack); err != nil || ack.Type != "watch_ack" || ack.Error != "" {
				b.Fatalf("watch reply %+v, %v", ack, err)
			}
		}
		latencies = append(latencies, time.Since(started))
	}
	elapsed := time.Since(start)
	bulkBytes := carrier.bulkWritten.Load() - startBytes
	b.StopTimer()
	slices.Sort(latencies)
	p95 := (95*len(latencies)+99)/100 - 1
	b.ReportMetric(float64(latencies[p95])/float64(time.Millisecond), "p95-ms")
	b.ReportMetric(float64(bulkBytes)/elapsed.Seconds()/(1024*1024), "bulk-wire-MiB/s")
}

// The local TCP buffers let the client pipeline requests. Pacing large
// replies models competition for a shared bottleneck without slowing metadata.
type pacedMountReplies struct {
	net.Conn
	firstData   func()
	bulkBytes   int
	bulkWritten atomic.Uint64
}

func (c *pacedMountReplies) Write(p []byte) (int, error) {
	if len(p) < 16<<10 {
		return c.Conn.Write(p)
	}
	c.bulkBytes += len(p)
	if c.bulkBytes >= files.MountSFTPDataLength {
		c.bulkBytes -= files.MountSFTPDataLength
		time.Sleep(2 * time.Millisecond)
	}
	n, err := c.Conn.Write(p)
	c.bulkWritten.Add(uint64(n))
	c.firstData()
	return n, err
}

//go:build linux || windows

package files

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sftp "github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
)

func TestMountRequestSchedulerReservesMetadataCapacity(t *testing.T) {
	s := &mountRequestScheduler{ctx: t.Context(), timeout: 20 * time.Millisecond}
	largeRead := &sshfx.ReadPacket{Length: mountSFTPDataLength}
	var releases []func()
	for range mountRequestWindows[mountData] {
		sent, release, err := s.admit(nil, nil, largeRead)
		if err != nil {
			t.Fatal(err)
		}
		sent()
		releases = append(releases, release)
	}

	for _, packet := range []sshfx.PacketMarshaller{&sshfx.StatPacket{}, &sshfx.ReadPacket{Length: 16}} {
		sent, release, err := s.admit(nil, nil, packet)
		if err != nil {
			t.Fatalf("interactive request was blocked behind the full data window: %v", err)
		}
		sent()
		release()
	}
	if _, _, err := s.admit(nil, nil, &sshfx.WritePacket{}); !errors.Is(err, errMountRequestQueueTimeout) {
		t.Fatalf("full data window should time out a queued write, got %v", err)
	}
	for _, release := range releases {
		release()
	}
	sent, release, err := s.admit(nil, nil, largeRead)
	if err != nil {
		t.Fatalf("queued timeout leaked a slot: %v", err)
	}
	sent()
	release()
}

func TestMountRequestSchedulerAlternatesEligibleQueues(t *testing.T) {
	s := &mountRequestScheduler{sending: true}
	m1, m2, d1, d2 := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.queues[mountMetadata].PushBack(m1)
	s.queues[mountMetadata].PushBack(m2)
	s.queues[mountData].PushBack(d1)
	s.queues[mountData].PushBack(d2)
	pending := []chan struct{}{m1, d1, m2, d2}
	for i, expected := range pending {
		s.sending = false
		s.grant()
		select {
		case <-expected:
		default:
			t.Fatalf("grant %d did not alternate queues and preserve their arrival order", i)
		}
		for _, later := range pending[i+1:] {
			select {
			case <-later:
				t.Fatal("granted more than one writer")
			default:
			}
		}
	}
}

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
	const block = 2 * mountSFTPDataLength
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
		_ = serveMountData(&pacedMountReplies{Conn: peer, firstData: sync.OnceFunc(func() { close(firstData) })})
	}()
	wire := newFTPWire(conn, conn)
	var client *sftp.Client
	if scheduled {
		client, err = newMountSFTPClient(b.Context(), wire, 0)
	} else {
		client, err = sftp.NewClientPipe(b.Context(), wire, wire, sftp.WithMaxDataLength(mountSFTPDataLength))
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
	if len(p) == mountSFTPDataLength {
		c.firstData()
		time.Sleep(2 * time.Millisecond)
	}
	return c.Conn.Write(p)
}

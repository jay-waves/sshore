package benchmarks

import (
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// These benchmarks isolate mux overhead on loopback TCP. They are not SSH or
// WAN benchmarks. Varying frame size also measures the latency trade-off when
// interactive traffic shares a session with bulk traffic.
func benchmarkMuxPair(b *testing.B, frame int) (*smux.Session, *smux.Session) {
	b.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	a, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	z, err := listener.Accept()
	if err != nil {
		a.Close()
		b.Fatal(err)
	}
	// Fix the resource limits for comparable runs. This benchmark explores frame
	// sizes and does not assert that either production mux uses these settings.
	config := smux.DefaultConfig()
	config.Version = 2
	config.MaxFrameSize = frame
	config.MaxStreamBuffer = 1024 * 1024
	config.MaxReceiveBuffer = 44 * config.MaxStreamBuffer
	config.KeepAliveInterval = 10 * time.Second
	config.KeepAliveTimeout = 60 * time.Second
	client, err := smux.Client(a, config)
	if err != nil {
		a.Close()
		z.Close()
		b.Fatal(err)
	}
	server, err := smux.Server(z, config)
	if err != nil {
		client.Close()
		z.Close()
		b.Fatal(err)
	}
	b.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

func BenchmarkRawLoopbackBulk(b *testing.B) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	sender, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer sender.Close()
	receiver, err := listener.Accept()
	if err != nil {
		b.Fatal(err)
	}
	defer receiver.Close()
	const size = 4 << 20
	payload := make([]byte, 32<<10)
	b.SetBytes(size)
	b.ResetTimer()
	for range b.N {
		done := make(chan error, 1)
		go func() { _, err := io.CopyN(io.Discard, receiver, size); done <- err }()
		for n := 0; n < size; n += len(payload) {
			if _, err := sender.Write(payload); err != nil {
				b.Fatal(err)
			}
		}
		if err := <-done; err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMuxBulk(b *testing.B) {
	for _, frame := range []int{4096, 16384, 32768, 65535} {
		for _, streams := range []int{1, 4} {
			b.Run(fmt.Sprintf("frame=%d/streams=%d", frame, streams), func(b *testing.B) {
				client, server := benchmarkMuxPair(b, frame)
				const size = 4 << 20
				payload := make([]byte, 32<<10)
				var senders, receivers []*smux.Stream
				for range streams {
					s, err := client.OpenStream()
					if err != nil {
						b.Fatal(err)
					}
					r, err := server.AcceptStream()
					if err != nil {
						b.Fatal(err)
					}
					senders, receivers = append(senders, s), append(receivers, r)
				}
				b.SetBytes(int64(size * streams))
				b.ResetTimer()
				for range b.N {
					var wg sync.WaitGroup
					errs := make(chan error, streams*2)
					for i := range streams {
						wg.Go(func() { _, err := io.CopyN(io.Discard, receivers[i], size); errs <- err })
						wg.Go(func() {
							for n := 0; n < size; n += len(payload) {
								if _, err := senders[i].Write(payload); err != nil {
									errs <- err
									return
								}
							}
							errs <- nil
						})
					}
					wg.Wait()
					close(errs)
					for err := range errs {
						if err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}

func BenchmarkMuxInteractiveUnderLoad(b *testing.B) {
	for _, frame := range []int{4096, 16384, 32768, 65535} {
		b.Run(fmt.Sprintf("frame=%d", frame), func(b *testing.B) {
			client, server := benchmarkMuxPair(b, frame)
			interactive, err := client.OpenStream()
			if err != nil {
				b.Fatal(err)
			}
			echo, err := server.AcceptStream()
			if err != nil {
				b.Fatal(err)
			}
			bulk, err := client.OpenStream()
			if err != nil {
				b.Fatal(err)
			}
			drain, err := server.AcceptStream()
			if err != nil {
				b.Fatal(err)
			}
			go io.Copy(io.Discard, drain)
			go func() {
				buf := make([]byte, 32<<10)
				for {
					if _, err := bulk.Write(buf); err != nil {
						return
					}
				}
			}()
			go func() { _, _ = io.Copy(echo, echo) }()
			var samples []int64
			one := []byte{1}
			b.ResetTimer()
			for range b.N {
				start := time.Now()
				if _, err := interactive.Write(one); err != nil {
					b.Fatal(err)
				}
				if _, err := io.ReadFull(interactive, one); err != nil {
					b.Fatal(err)
				}
				samples = append(samples, time.Since(start).Nanoseconds())
			}
			b.StopTimer()
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			b.ReportMetric(float64(samples[(len(samples)-1)*95/100])/1000, "p95-us")
			b.ReportMetric(float64(samples[(len(samples)-1)*99/100])/1000, "p99-us")
		})
	}
}

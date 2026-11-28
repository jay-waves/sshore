package benchmarks

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"testing"

	"golang.org/x/crypto/ssh"
)

func BenchmarkSSHHandshake(b *testing.B) {
	listener, signer := sshBenchmarkListener(b)
	var reads int
	b.ReportAllocs()
	for b.Loop() {
		client, banner, done := openBenchmarkSSH(b, listener, signer, false)
		reads += banner.reads
		_ = client.Close()
		if err := <-done; err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(reads)/float64(b.N), "banner-reads/op")
}

func BenchmarkSSHChannelRoundTrip(b *testing.B) {
	for _, scenario := range []struct {
		name   string
		size   int
		framed bool
	}{
		{"interactive", 64, false},
		{"bulk", 32 << 10, false},
		{"framed-bulk", (32 << 10) + 8, true},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			listener, signer := sshBenchmarkListener(b)
			client, _, done := openBenchmarkSSH(b, listener, signer, true)
			defer func() {
				_ = client.Close()
				if err := <-done; err != nil {
					b.Error(err)
				}
			}()
			channel, requests, err := client.OpenChannel("session", nil)
			if err != nil {
				b.Fatal(err)
			}
			defer channel.Close()
			go ssh.DiscardRequests(requests)
			sent := bytes.Repeat([]byte{0x5a}, scenario.size)
			received := make([]byte, scenario.size)
			write := channel.Write
			if scenario.framed {
				write = benchmarkFrameWriter(channel, sent)
			}
			b.SetBytes(2 * int64(scenario.size))
			b.ReportAllocs()
			for b.Loop() {
				if n, err := write(sent); err != nil || n != len(sent) {
					b.Fatalf("write: %d bytes, %v", n, err)
				}
				if _, err := io.ReadFull(channel, received); err != nil {
					b.Fatal(err)
				}
				if !bytes.Equal(received, sent) {
					b.Fatal("SSH channel did not echo the payload")
				}
			}
		})
	}
}

func benchmarkFrameWriter(channel ssh.Channel, frame []byte) func([]byte) (int, error) {
	for i := range 8 {
		frame[i] = byte(i)
	}
	parts := [][]byte{frame[:8], nil, frame[8:]}
	if writer, ok := channel.(interface{ WriteBuffers([][]byte) (int, error) }); ok {
		return func([]byte) (int, error) { return writer.WriteBuffers(parts) }
	}
	joined := make([]byte, len(frame))
	return func([]byte) (int, error) {
		offset := 0
		for _, part := range parts {
			offset += copy(joined[offset:], part)
		}
		return channel.Write(joined)
	}
}

func sshBenchmarkListener(b *testing.B) (net.Listener, ssh.Signer) {
	b.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		b.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = listener.Close() })
	return listener, signer
}

func openBenchmarkSSH(b *testing.B, listener net.Listener, signer ssh.Signer, echo bool) (*ssh.Client, *countedSSHBanner, <-chan error) {
	b.Helper()
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer peer.Close()
		config := &ssh.ServerConfig{NoClientAuth: true} // Ephemeral, loopback-only benchmark.
		config.AddHostKey(signer)
		server, incoming, requests, err := ssh.NewServerConn(peer, config)
		if err != nil {
			done <- err
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(requests)
		if echo {
			request, ok := <-incoming
			if ok {
				channel, messages, err := request.Accept()
				if err != nil {
					done <- err
					return
				}
				go ssh.DiscardRequests(messages)
				_, err = io.Copy(channel, channel)
				_ = channel.Close()
				done <- err
				return
			}
		}
		_ = server.Wait()
		done <- nil
	}()
	banner := &countedSSHBanner{Conn: raw}
	conn, channels, requests, err := ssh.NewClientConn(banner, listener.Addr().String(),
		&ssh.ClientConfig{User: "benchmark", HostKeyCallback: ssh.FixedHostKey(signer.PublicKey())})
	if err != nil {
		_ = raw.Close()
		b.Fatal(err)
	}
	return ssh.NewClient(conn, channels, requests), banner, done
}

type countedSSHBanner struct {
	net.Conn
	reads    int
	complete bool
}

func (c *countedSSHBanner) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if !c.complete {
		c.reads++
		c.complete = bytes.IndexByte(p[:n], '\n') >= 0
	}
	return n, err
}

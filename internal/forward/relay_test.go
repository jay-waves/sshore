package forward

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

func awaitTransport(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("transport did not stop")
	}
}

func TestBridgeEOFJoinsBothDirections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, peerA := net.Pipe()
		b, peerB := net.Pipe()
		defer peerA.Close()
		defer peerB.Close()
		done := make(chan struct{})
		go func() { Bridge(a, b); close(done) }()
		peerA.Close()
		awaitTransport(t, done)
		peerB.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := peerB.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("opposite endpoint survived EOF: %v", err)
		}
	})
}

func TestSpliceCancellationJoinsBlockedCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, blockedWrite := range []bool{false, true} {
			a, peerA := net.Pipe()
			b, peerB := net.Pipe()
			defer peerA.Close()
			defer peerB.Close()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { Splice(ctx, a, b); close(done) }()
			if blockedWrite {
				// The relay reads the byte, then blocks writing to the unread peerB.
				peerA.SetWriteDeadline(time.Now().Add(time.Second))
				if _, err := peerA.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			awaitTransport(t, done)
			for _, c := range []net.Conn{peerA, peerB} {
				c.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := c.Read(make([]byte, 1)); err != io.EOF {
					t.Fatalf("endpoint survived cancellation: %v", err)
				}
			}
		}
	})
}

type failingTransport struct{ net.Conn }

func (f failingTransport) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestSpliceErrorAbortsOtherDirection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, peerA := net.Pipe()
		b, peerB := net.Pipe()
		defer peerA.Close()
		defer peerB.Close()
		done := make(chan struct{})
		go func() { Splice(context.Background(), failingTransport{a}, b); close(done) }()
		awaitTransport(t, done)
	})
}

func tcpTransportPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b, err := listener.Accept()
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	for _, c := range []net.Conn{a, b} {
		c.SetDeadline(time.Now().Add(3 * time.Second))
		t.Cleanup(func() { c.Close() })
	}
	return a.(*net.TCPConn), b.(*net.TCPConn)
}

func TestSplicePreservesResponseAfterHalfClose(t *testing.T) {
	a, peerA := tcpTransportPair(t)
	b, peerB := tcpTransportPair(t)
	done := make(chan struct{})
	go func() { Splice(context.Background(), a, b); close(done) }()
	if _, err := peerA.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	peerA.CloseWrite()
	if data, err := io.ReadAll(peerB); err != nil || string(data) != "request" {
		t.Fatalf("request = %q, %v", data, err)
	}
	if _, err := peerB.Write([]byte("response")); err != nil {
		t.Fatal(err)
	}
	peerB.CloseWrite()
	if data, err := io.ReadAll(peerA); err != nil || string(data) != "response" {
		t.Fatalf("response = %q, %v", data, err)
	}
	awaitTransport(t, done)
}

package files

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

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

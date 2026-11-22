package files

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	sftp "github.com/pkg/sftp/v2"
)

func TestSFTPRequestTimeoutIgnoresHealthySibling(t *testing.T) {
	wire := newFTPWire(strings.NewReader(""), nopWriteCloser{io.Discard})
	packet := make([]byte, 5)
	packet[0] = 3
	binary.BigEndian.PutUint32(packet[1:], 1)
	if e := wire.record(packet); e != nil {
		t.Fatal(e)
	}
	fired := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { fired <- wire.watchRequests(ctx, 30*time.Millisecond) }()
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
	case err := <-fired:
		if err == nil || !strings.Contains(err.Error(), "no retry") {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("request failed to time out")
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
func TestSFTPIdleDoesNotTimeout(t *testing.T) {
	wire := newFTPWire(strings.NewReader(""), nopWriteCloser{io.Discard})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go func() {
		if err := wire.watchRequests(ctx, 10*time.Millisecond); err != nil {
			cancel(err)
		}
	}()
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
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go func() {
		if err := wire.watchRequests(ctx, 30*time.Millisecond); err != nil {
			cancel(err)
			b.Close()
		}
	}()
	client, e := sftp.NewClientPipe(t.Context(), wire, wire)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	done := make(chan error, 1)
	go func() { _, e := client.RealPath("."); done <- e }()
	select {
	case e := <-done:
		if e == nil || context.Cause(ctx) == nil {
			t.Fatal(e, context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("pending SFTP call leaked")
	}
}

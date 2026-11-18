package wire

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func TestMuxStalledFilesLeaveControlCapacity(t *testing.T) {
	a, b := net.Pipe()
	config := MuxConfig()
	client, err := smux.Client(a, config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := smux.Server(b, config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	// Fill every permitted file stream without consuming any of its data.
	block := make([]byte, config.MaxStreamBuffer)
	for i := 0; i < MaxFileStreams; i++ {
		out, err := client.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		in, err := server.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		out.SetWriteDeadline(time.Now().Add(5 * time.Second))
		in.SetReadDeadline(time.Now().Add(5 * time.Second))
		// The first read advertises the configured window to the sender.
		if _, err := out.Write([]byte{0}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(in, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	out, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	in, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	out.SetWriteDeadline(time.Now().Add(5 * time.Second))
	in.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := out.Write([]byte("control")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	if _, err := io.ReadFull(in, buf); err != nil || string(buf) != "control" {
		t.Fatal("stalled files blocked control traffic", string(buf), err)
	}
}

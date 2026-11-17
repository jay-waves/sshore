package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/xtaci/smux"
)

type fragmentedReader struct{ io.Reader }

func (r fragmentedReader) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return r.Reader.Read(b)
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(b []byte) (int, error) {
	if len(b) > 2 {
		b = b[:2]
	}
	return w.Buffer.Write(b)
}
func TestProtocolBinaryAndFragmentation(t *testing.T) {
	data := make([]byte, chunkSize)
	for i := range data {
		data[i] = byte(i)
	}
	var b shortWriter
	for _, m := range []message{{Type: "attach", SoftwareVersion: version, Term: "xterm", Cols: 90, Rows: 30}, {Type: "input", Data: data}, {Type: "output", Data: []byte("\x00\xff\x1b[31m")}} {
		if err := writeMessage(&b, m); err != nil {
			t.Fatal(err)
		}
	}
	r := fragmentedReader{&b.Buffer}
	m, err := readMessage(r)
	if err != nil || m.Cols != 90 || m.SoftwareVersion != version {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = readMessage(r)
	if err != nil || m.Type != "input" || !bytes.Equal(m.Data, data) {
		t.Fatal("binary frame mismatch", err)
	}
	m, err = readMessage(r)
	if err != nil || !bytes.Equal(m.Data, []byte("\x00\xff\x1b[31m")) {
		t.Fatal("terminal frame mismatch", err)
	}
}
func TestProtocolRejectsMalformedFrames(t *testing.T) {
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[1:], maxFrame+1)
	for _, b := range [][]byte{header, {4, 0, 0, 0, 0}, {3, 0, 0, 0, 1, 0}, {1, 0, 0, 0, 2, 0}, {0, 0, 0, 0, 0}} {
		if _, err := readMessage(bytes.NewReader(b)); err == nil {
			t.Fatalf("accepted %v", b)
		}
	}
	for _, body := range []string{`{}`, `{"type":"input"}`, `{"type":"output"}`, `{"type":"chunk"}`} {
		frame := make([]byte, 5+len(body))
		binary.BigEndian.PutUint32(frame[1:], uint32(len(body)))
		copy(frame[5:], body)
		if _, err := readMessage(bytes.NewReader(frame)); err == nil {
			t.Fatalf("accepted reserved control metadata: %s", body)
		}
	}
}
func TestOptions(t *testing.T) {
	o, err := parseOptions([]string{"launch", "myhost", "build", "--server", "/a b/sshore", "--", "-J", "jump"})
	if err != nil || o.session != "build" || o.host != "myhost" || len(o.ssh) != 2 {
		t.Fatalf("%+v %v", o, err)
	}
	for _, name := range []string{"", "../bad", "-bad", "space name", "\u4e2d\u6587"} {
		if validSession(name) == nil {
			t.Fatal("accepted", name)
		}
	}
	for _, args := range [][]string{{"launch", "host", "-s"}, {"launch", "-evil"}, {"launch", "host", "name", "unexpected"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestProtocolUnicodeError(t *testing.T) {
	want := "无法打开 中文文件.txt；literal\\u4e2d.txt"
	var b bytes.Buffer
	if err := writeMessage(&b, message{Type: "error", Error: want}); err != nil {
		t.Fatal(err)
	}
	m, err := readMessage(&b)
	if err != nil || m.Error != want {
		t.Fatalf("got %q, %v; want %q", m.Error, err, want)
	}
}

func muxPair(t *testing.T) (*smux.Session, *smux.Session) {
	t.Helper()
	a, b := net.Pipe()
	client, err := smux.Client(a, transportio.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	server, err := smux.Server(b, transportio.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

func TestSpawnTransportKeepsIndependentAttachmentStreams(t *testing.T) {
	frontend, relay := muxPair(t)
	remote, daemon := muxPair(t)
	go relaySpawnStreams(relay, remote)
	go func() {
		for {
			stream, err := daemon.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				defer stream.Close()
				m, err := readMessage(stream)
				if err == nil {
					_ = writeMessage(stream, m)
				}
			}()
		}
	}()
	o := options{command: "attach", host: "must-not-authenticate", session: "parent", instance: "parent-instance",
		reusable: &clientTransport{session: "parent", instance: "parent-instance", open: func(context.Context) (io.ReadWriteCloser, error) {
			return frontend.OpenStream()
		}}}
	for _, kind := range []string{"spawn_route_connect", "spawn_route_confirm", "mux"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, err := openClientTransportContext(ctx, &o)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		err = writeMessage(c, message{Type: kind})
		if err == nil {
			var reply message
			reply, err = readMessage(c)
			if err == nil && reply.Type != kind {
				err = fmt.Errorf("unexpected reply: %s", reply.Type)
			}
		}
		c.Close()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	if frontend.IsClosed() || remote.IsClosed() {
		t.Fatal("attachment close ended the shared transport")
	}
}
func TestClientMuxRejectsDifferentOrUnknownBuild(t *testing.T) {
	for _, peer := range []string{"0.0.0-old", ""} {
		t.Run(peer, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			_ = a.SetDeadline(time.Now().Add(3 * time.Second))
			done := make(chan error, 1)
			go func() {
				m, err := readMessage(b)
				if err == nil && m.SoftwareVersion != version {
					err = fmt.Errorf("client omitted its software version")
				}
				if err == nil {
					err = writeMessage(b, message{Type: "mux_ready", SoftwareVersion: peer})
				}
				done <- err
			}()
			mux, err := clientMux(a)
			if mux != nil {
				mux.Close()
				t.Fatal("accepted incompatible daemon")
			}
			if err == nil || !strings.Contains(err.Error(), "remote shell daemon") || !strings.Contains(err.Error(), "new shell name") {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAttachmentRejectsTunnelStreams(t *testing.T) {
	client, server := muxPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveApprovedStreams(ctx, server)
	for _, kind := range []string{"forward_connect", "forward_control"} {
		st, e := client.OpenStream()
		if e != nil {
			t.Fatal(e)
		}
		st.SetDeadline(time.Now().Add(time.Second))
		if e = writeMessage(st, message{Type: kind, Action: "add"}); e != nil {
			t.Fatal(e)
		}
		if m, e := readMessage(st); e == nil {
			t.Fatalf("obsolete attachment tunnel accepted: %+v", m)
		}
		st.Close()
	}
}

func TestApprovalMismatchDoesNotWriteFile(t *testing.T) {
	client, server := muxPair(t)
	target := filepath.Join(t.TempDir(), "secret")
	go func() {
		st, err := server.AcceptStream()
		if err != nil {
			return
		}
		defer st.Close()
		_, _ = readMessage(st)
		_ = writeMessage(st, message{Type: "offer", Name: "other", Size: 3})
	}()
	err := executeApprovedFile(context.Background(), client, operation{Kind: "download", Path: "/tmp/a", Name: "a", Size: 3, LocalPath: target}, false)
	if err == nil {
		t.Fatal("mismatch accepted")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("wrote unapproved file", err)
	}
}

func TestChangedUploadIsNotSent(t *testing.T) {
	client, server := muxPair(t)
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		st, err := server.AcceptStream()
		if err != nil {
			result <- err
			return
		}
		defer st.Close()
		_, err = readMessage(st)
		result <- err
	}()
	err := executeApprovedFile(context.Background(), client, operation{Kind: "upload", Path: "/tmp", Name: "source", Size: 1, LocalPath: path}, true)
	if err == nil {
		t.Fatal("changed upload accepted")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("unapproved upload sent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not close")
	}
}

func TestSpawnConfirmationCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	parent := options{session: "parent", instance: "instance", reusable: &clientTransport{
		session: "parent", instance: "instance", open: func(context.Context) (io.ReadWriteCloser, error) { return client, nil },
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- confirmSpawnRoute(ctx, parent, "token", nil) }()
	request, err := readMessage(server)
	if err != nil || request.Type != "spawn_route_confirm" {
		t.Fatalf("confirmation request: %+v, %v", request, err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled confirmation succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("confirmation blocked after cancellation")
	}
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"

	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func TestSessionTargetCycle(t *testing.T) {
	first := sessionTarget{Name: "a", Instance: strings.Repeat("a", 32)}
	last := sessionTarget{Name: "z", Instance: strings.Repeat("b", 32), Hosted: true}
	targets := []sessionTarget{last, first}
	for _, tc := range []struct {
		current  string
		previous bool
		want     sessionTarget
	}{{"m", false, last}, {"z", false, first}, {"a", true, last}} {
		if got := adjacentSessionTarget(tc.current, targets, tc.previous); got != tc.want {
			t.Fatalf("current %q, previous=%t: got %+v, want %+v", tc.current, tc.previous, got, tc.want)
		}
	}
	if targets[0] != last || targets[1] != first {
		t.Fatal("mutated input")
	}
	if got := adjacentSessionTarget("only", nil, false); got.Name != "" {
		t.Fatal("single session moved")
	}
}

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
func TestProtocolControlFragmentation(t *testing.T) {
	var b shortWriter
	for _, m := range []message{{Type: "attach", SoftwareVersion: version, Term: "xterm", Cols: 90, Rows: 30}, {Type: "input_barrier", TerminalOffset: 65537}} {
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
	if err != nil || m.Type != "input_barrier" || m.TerminalOffset != 65537 {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestProtocolJSONLines(t *testing.T) {
	var out bytes.Buffer
	for range 2 {
		if err := writeMessage(&out, message{Type: "bye"}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := out.String(), "{\"type\":\"bye\"}\n{\"type\":\"bye\"}\n"; got != want {
		t.Fatalf("encoded %q, want %q", got, want)
	}
}

func TestProtocolRejectsMalformedFrames(t *testing.T) {
	for _, body := range []string{"\n", "{\n", `{}
`, `{"type":"input"}
`, `{"type":"output"}
`, `{"type":"chunk"}
`, strings.Repeat(" ", maxControlMessage+1) + "\n"} {
		for _, reader := range []io.Reader{strings.NewReader(body), bufio.NewReader(strings.NewReader(body))} {
			if _, err := readMessage(reader); err == nil {
				t.Fatalf("accepted invalid control line of length %d", len(body))
			}
		}
	}
	for _, reader := range []io.Reader{strings.NewReader(`{"type":"bye"}`), bufio.NewReader(strings.NewReader(`{"type":"bye"}`))} {
		if _, err := readMessage(reader); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated read: %v", err)
		}
	}
}

func TestControlHandshakePreservesRawBytes(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	raw := []byte{0, 255, 13, 10, 7}
	go func() {
		_, _ = b.Write(append([]byte("{\"type\":\"sftp\"}\n"), raw...))
	}()
	stream := ownClientTransport(context.Background(), a, nil)
	defer stream.Close()
	m, err := readMessage(stream)
	if err != nil || m.Type != "sftp" {
		t.Fatalf("handshake: %+v %v", m, err)
	}
	got := make([]byte, len(raw))
	if _, err := io.ReadFull(stream, got); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("raw tail: %x %v", got, err)
	}
}
func TestOptions(t *testing.T) {
	o, err := parseOptions([]string{"sh", "myhost:", "--server", "/a b/sshore", "--", "-J", "jump"})
	if err != nil || o.session != "" || o.host != "myhost" || len(o.ssh) != 2 {
		t.Fatalf("%+v %v", o, err)
	}
	for _, name := range []string{"", "../bad", "-bad", "space name", "\u4e2d\u6587"} {
		if validSession(name) == nil {
			t.Fatal("accepted", name)
		}
	}
	for _, args := range [][]string{{"sh", "host", "-s"}, {"sh", "-evil"}, {"sh", "host", "name", "unexpected"}} {
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
	client, err := smux.Client(a, relayMuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	server, err := smux.Server(b, relayMuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

func TestFrontendDaemonMuxKeepsIndependentTaskEndpoints(t *testing.T) {
	frontend, relay := muxPair(t)
	remote, daemon := muxPair(t)
	go relayRemoteStreams(relay, func() (net.Conn, error) { return remote.OpenStream() })
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
		reusable: &clientTransport{session: "parent", instance: "parent-instance", open: func(context.Context) (net.Conn, error) {
			return frontend.OpenStream()
		}}}
	for _, kind := range []string{"shell_route_connect", "shell_route_confirm", "terminal_open"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, err := openClientTransportContext(ctx, &o, nil)
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
func TestApprovalMismatchDoesNotWriteFile(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	target := filepath.Join(t.TempDir(), "secret")
	go func() {
		defer server.Close()
		_, _ = readMessage(server)
		_ = writeMessage(server, message{Type: "offer", Name: "other", Size: 3})
	}()
	open := func() (net.Conn, error) { return client, nil }
	err := executeApprovedFile(context.Background(), open, newApprovedFile(operation{Kind: "download", Path: "/tmp/a", Name: "a", Size: 3}, target, nil), false)
	if err == nil {
		t.Fatal("mismatch accepted")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("wrote unapproved file", err)
	}
}

func TestApprovedUploadRequiresOpenedSource(t *testing.T) {
	open := func() (net.Conn, error) {
		t.Fatal("unapproved upload opened an endpoint")
		return nil, nil
	}
	err := executeApprovedFile(context.Background(), open, newApprovedFile(operation{Kind: "upload", Path: "/tmp"}, "", nil), true)
	if err == nil {
		t.Fatal("upload without an opened source accepted")
	}
}

func TestApprovedFileEndpointsReuseAuthenticatedLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	instance := strings.Repeat("a", 32)
	approval := strings.Repeat("b", 32)
	var opened, released int
	checked := make(chan message, 2)
	lease := &clientTransport{instance: instance, close: func() { released++ }, open: func(context.Context) (net.Conn, error) {
		opened++
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			m, err := readMessage(server)
			if err == nil {
				checked <- m
				_ = writeMessage(server, message{Type: "resource_ready", SoftwareVersion: version})
			}
		}()
		return client, nil
	}}
	transports := clientTransports{lease: false}
	o := options{command: "attach", instance: instance, reusable: lease}
	request := message{Type: "route_files", Name: "source", PID: 100, ApprovalID: approval}
	for range 2 {
		endpoint, err := openResourceEndpoint(ctx, &o, request, transports)
		if err != nil {
			t.Fatal(err)
		}
		endpoint.Close()
		m := <-checked
		if m.Type != request.Type || m.Instance != instance || m.ApprovalID != approval || m.PID != request.PID || m.SoftwareVersion != version {
			t.Fatalf("file endpoint skipped its approval handshake: %+v", m)
		}
		if released != 0 {
			t.Fatal("closing one endpoint ended its authenticated lease")
		}
	}
	transports.releaseUnreferenced(false)
	if opened != 2 || released != 1 {
		t.Fatalf("opened endpoints=%d, released leases=%d", opened, released)
	}
}

func TestSpawnConfirmationCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	parent := options{session: "parent", instance: "instance", reusable: &clientTransport{
		session: "parent", instance: "instance", open: func(context.Context) (net.Conn, error) { return client, nil },
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- confirmShellRoute(ctx, parent, "token", nil) }()
	request, err := readMessage(server)
	if err != nil || request.Type != "shell_route_confirm" {
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

func TestFrontendReleasesUnreachableTransports(t *testing.T) {
	ctx := context.Background()
	owned := make(clientTransports)
	open := func(context.Context) (net.Conn, error) { a, b := net.Pipe(); b.Close(); return a, nil }
	var parentClosed, childrenClosed int
	var parent options
	stream, err := owned.openTaskStream(ctx, &parent, open, func() { parentClosed++ })
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	for i := 1; i <= 32; i++ {
		child := options{routeParent: &parent}
		c, err := owned.openTaskStream(ctx, &child, open, func() { childrenClosed++ })
		if err != nil {
			t.Fatal(err)
		}
		owned.retainRoute(&child)
		owned.releaseUnreferenced(false)
		if len(owned) != 2 {
			t.Fatal("lost a route ancestor")
		}
		c.Close()
		owned.retainRoute(&parent)
		owned.releaseUnreferenced(false)
		if parentClosed != 0 || childrenClosed != i || len(owned) != 1 {
			t.Fatal("return retained an obsolete transport or closed parent")
		}
	}
	owned.releaseUnreferenced(false)
	if parentClosed != 1 {
		t.Fatal("parent was not released exactly once")
	}
	closed := 0
	_, err = owned.openTaskStream(ctx, &parent, func(context.Context) (net.Conn, error) { return nil, io.EOF }, func() { closed++ })
	if err != io.EOF || closed != 1 || len(owned) != 0 {
		t.Fatal("failed first stream retained transport")
	}
}

func TestSessionDisconnectReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timeout"},
		{smux.ErrTimeout, "timeout"},
		{context.Canceled, "interrupted"},
		{io.EOF, "connection closed"},
		{io.ErrUnexpectedEOF, "connection closed"},
		{net.ErrClosed, "connection closed"},
		{fmt.Errorf("detach response timed out"), "timeout"},
		{fmt.Errorf("connection reset by peer"), "connection error"},
	} {
		if got := sessionDisconnectReason(fmt.Errorf("connection lost: %w", tc.err)); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.err, got, tc.want)
		}
	}
}

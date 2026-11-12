package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	_ = a.SetDeadline(time.Now().Add(5 * time.Second))
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	return a.(*net.TCPConn), b.(*net.TCPConn)
}
func awaitForward(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not terminate")
	}
}
func TestForwardHalfCloseAndCancel(t *testing.T) {
	for _, cancelAfterEOF := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelAfterEOF), func(t *testing.T) {
			client, server := muxPair(t)
			a, err := client.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			b, err := server.AcceptStream()
			if err != nil {
				t.Fatal(err)
			}
			app, local := tcpPair(t)
			service, remote := tcpPair(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			localDone, remoteDone := make(chan struct{}), make(chan struct{})
			go func() { defer close(localDone); defer a.Close(); relayForward(ctx, local, a) }()
			go func() { defer close(remoteDone); defer b.Close(); relayForward(context.Background(), remote, b) }()
			payload := bytes.Repeat([]byte{0, 255, 1, 2}, 50000)
			go func() { _, _ = app.Write(payload); _ = app.CloseWrite() }()
			got, err := io.ReadAll(service)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("request: %d %v", len(got), err)
			}
			if cancelAfterEOF {
				// The service leaves its response side open indefinitely.
				cancel()
				awaitForward(t, localDone)
				awaitForward(t, remoteDone)
			} else {
				go func() { _, _ = service.Write(payload); _ = service.CloseWrite() }()
				got, err = io.ReadAll(app)
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("response after EOF: %d %v", len(got), err)
				}
				awaitForward(t, localDone)
				awaitForward(t, remoteDone)
			}
		})
	}
}
func TestForwardManagerLifecycle(t *testing.T) {
	client, server := muxPair(t)
	f := newForwardManager(context.Background(), client)
	defer f.close()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	request := message{Action: "add", Port: port}
	if r := f.request(request); r.Error == "" {
		t.Fatal("occupied port accepted")
	}
	ln.Close()
	if r := f.request(request); r.Error != "" {
		t.Fatal(r.Error)
	}
	if r := f.request(request); r.Name != "Already forwarding" {
		t.Fatal(r)
	}
	if r := f.request(message{Action: "list"}); len(r.Ports) != 1 || r.Ports[0] != port {
		t.Fatal(r)
	}
	c, err := net.DialTimeout("tcp4", loopback(port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := readMessage(st)
	if err != nil || m.Type != "forward_connect" || m.Port != port {
		t.Fatal(m, err)
	}
	if err = writeMessage(st, message{Type: "forward_ready"}); err != nil {
		t.Fatal(err)
	}
	if r := f.request(message{Action: "stop", Port: port}); r.Error != "" {
		t.Fatal(r)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err = c.Read(b[:]); err == nil {
		t.Fatal("connection survived stop")
	}
	if r := f.request(request); r.Error != "" {
		t.Fatal("restart:", r)
	}
	f.close()
	ln, err = net.Listen("tcp4", loopback(port))
	if err != nil {
		t.Fatal("listener survived close:", err)
	}
	ln.Close()
	if r := f.request(request); r.Error == "" {
		t.Fatal("closed manager accepted request")
	}
}
func TestForwardArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"0"}, {"65536"}, {"-1"}, {"+1"}, {"localhost:80"}, {"7777:host:0"}, {"7777:host name:80"}, {"--stop"}, {"--list", "80"}, {"80", "81"}} {
		if _, err := parseForward(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, args := range [][]string{{"1"}, {"65535"}, {"7777:6666"}, {"7777:192.168.1.20:6666"}, {"--stop", "3000"}, {"--list"}} {
		if _, err := parseForward(args); err != nil {
			t.Fatal(args, err)
		}
	}
	m, err := parseForward([]string{"7777:192.168.1.20:6666"})
	if err != nil || m.Port != 7777 || m.RemoteHost != "192.168.1.20" || m.RemotePort != 6666 {
		t.Fatalf("parsed target: %+v (%v)", m, err)
	}
	m, err = parseForward([]string{"7777:6666"})
	if err != nil || m.RemoteHost != "127.0.0.1" || m.RemotePort != 6666 {
		t.Fatalf("parsed shorthand: %+v (%v)", m, err)
	}
	o, err := parseOptions([]string{"new", "host", "-L", "127.0.0.1:8080:localhost:80"})
	if err != nil || len(o.ssh) != 2 {
		t.Fatal(o, err)
	}
	cmd := sshCommand(o)
	if cmd.Args[7] != "-L" || cmd.Args[8] != o.ssh[1] {
		t.Fatal(cmd.Args)
	}
}

func TestForwardPortLimit(t *testing.T) {
	client, _ := muxPair(t)
	f := newForwardManager(context.Background(), client)
	defer f.close()
	listeners := make([]net.Listener, maxForwardPorts+1)
	for i := range listeners {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners[i] = ln
		t.Cleanup(func() { ln.Close() })
	}
	for i, ln := range listeners {
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		r := f.request(message{Action: "add", Port: port})
		if (r.Error != "") != (i == maxForwardPorts) {
			t.Fatalf("rule %d: %+v", i, r)
		}
	}
}
func TestForwardNativeOptions(t *testing.T) {
	o, err := parseOptions([]string{"new", "host", "-L8080:localhost:80", "-L", "8081:localhost:81"})
	if err != nil || len(o.ssh) != 4 || o.ssh[1] != "8080:localhost:80" || o.ssh[3] != "8081:localhost:81" {
		t.Fatal(o, err)
	}
}

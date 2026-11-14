package main

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestLocalForwardDirections(t *testing.T) {
	for _, tc := range []struct{ direction, flag, spec string }{
		{"to", "-L", "127.0.0.1:8080:127.0.0.1:3000"},
		{"from", "-R", "127.0.0.1:3000:127.0.0.1:8080"},
	} {
		args, e := localForwardArgs([]string{"work", "8080", tc.direction, "3000"})
		if e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(strings.Join(args, " "), tc.flag+" "+tc.spec) {
			t.Fatal(args)
		}
	}
	for _, args := range [][]string{{"work", "3000"}, {"work", "8080:3000"}, {"work", "8080", "to", "3000", "extra"}} {
		if _, e := localForwardArgs(args); e == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestReverseForwardGrantAndLifecycle(t *testing.T) {
	client, server := muxPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newForwardManager(ctx, client)
	defer f.close()
	go serveForwardStreams(ctx, server)
	go func() {
		for {
			st, e := client.AcceptStream()
			if e != nil {
				return
			}
			m, e := readMessage(st)
			if e != nil {
				st.Close()
				continue
			}
			go f.handleReverse(st, m)
		}
	}()
	service, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer service.Close()
	target := service.Addr().(*net.TCPAddr).Port
	candidate, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := candidate.Addr().(*net.TCPAddr).Port
	candidate.Close()
	request := message{Action: "add", Reverse: true, Port: port, RemotePort: target, RemoteHost: "127.0.0.1"}
	// An unapproved server cannot get a ready response or dial a local service.
	probe := func(m message) {
		t.Helper()
		st, e := server.OpenStream()
		if e != nil {
			t.Fatal(e)
		}
		defer st.Close()
		st.SetDeadline(time.Now().Add(time.Second))
		m.Type = "forward_connect"
		if e = writeMessage(st, m); e != nil {
			t.Fatal(e)
		}
		if r, e := readMessage(st); e == nil {
			t.Fatalf("unauthorized reverse accepted: %+v", r)
		}
	}
	probe(request)
	// A reverse request must not reuse a normal grant, even with a malformed port.
	normal := message{Action: "add", Port: port, RemoteHost: "127.0.0.1", RemotePort: target}
	if r := f.request(normal); r.Error != "" {
		t.Fatal(r.Error)
	}
	forged := request
	probe(forged)
	forged.Port = -port
	probe(forged)
	normal.Action = "stop"
	if r := f.request(normal); r.Error != "" {
		t.Fatal(r.Error)
	}
	if r := f.request(request); r.Error != "" {
		t.Fatal(r.Error)
	}
	bad := request
	bad.RemotePort++
	probe(bad)
	bad = request
	bad.RemoteHost = "localhost"
	probe(bad)
	if r := f.request(message{Action: "list"}); len(r.Forwards) != 1 || !r.Forwards[0].Reverse {
		t.Fatalf("list: %+v", r)
	}
	app, e := net.DialTimeout("tcp4", loopback(port), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer app.Close()
	app.SetDeadline(time.Now().Add(3 * time.Second))
	service.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	peer, e := service.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer peer.Close()
	peer.SetDeadline(time.Now().Add(3 * time.Second))
	if _, e = app.Write([]byte("request")); e != nil {
		t.Fatal(e)
	}
	b := make([]byte, 7)
	if _, e = io.ReadFull(peer, b); e != nil || string(b) != "request" {
		t.Fatal(string(b), e)
	}
	if _, e = peer.Write([]byte("reply")); e != nil {
		t.Fatal(e)
	}
	b = make([]byte, 5)
	if _, e = io.ReadFull(app, b); e != nil || string(b) != "reply" {
		t.Fatal(string(b), e)
	}
	request.Action = "stop"
	if r := f.request(request); r.Error != "" {
		t.Fatal(r.Error)
	}
	if _, e = peer.Read(make([]byte, 1)); e != io.EOF {
		t.Fatalf("connection survived stop: %v", e)
	}
	probe(request)
	if c, e := net.DialTimeout("tcp4", loopback(port), time.Second); e == nil {
		c.Close()
		t.Fatal("listener survived stop")
	}
}

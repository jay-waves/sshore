package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestForwardHelperAndRemoteDial(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("main")
	f := newForwardManager(context.Background(), b.mux)
	defer f.close()
	go func() {
		for {
			st, err := b.mux.AcceptStream()
			if err != nil {
				return
			}
			m, err := readMessage(st)
			if err != nil {
				st.Close()
				continue
			}
			if m.Type == "approval_request" {
				_ = writeMessage(st, message{Type: "queued"})
				if ack, e := readMessage(st); e == nil && ack.Type == "queued_ack" {
					f.request(message{Action: "add", Port: m.Operations[0].Port})
				}
				st.Close()
			} else if m.Type == "forward_connect" && m.Reverse {
				go f.handleReverse(st, m)
			} else {
				f.handle(st, m)
			}
		}
	}()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(e.bin, append([]string{"forward"}, args...)...)
		cmd.Env = append(e.env, "SSHORE_SOCKET="+filepath.Join(e.dir, "main.sock"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper: %v %s", err, out)
		}
		return string(out)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if out := run(strconv.Itoa(port), "from", strconv.Itoa(port)); !strings.Contains(out, "Request queued") {
		t.Fatal(out)
	}
	if out := run("--list"); !strings.Contains(out, loopback(port)) {
		t.Fatal(out)
	}
	if out := run(strconv.Itoa(port), "from", strconv.Itoa(port)); !strings.Contains(out, "Request queued") {
		t.Fatal(out)
	}
	run("--stop", strconv.Itoa(port), "from", strconv.Itoa(port))
	if out := run("--list"); !strings.Contains(out, "No dynamic forwards") {
		t.Fatal(out)
	}

	// Client and daemon share this test host, so exercise remote dialing on a
	// separate service port rather than recursively connecting a same-port rule.
	service, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	st, err := b.mux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sendTest(t, st, message{Type: "forward_connect", Port: service.Addr().(*net.TCPAddr).Port})
	if m := recv(t, st); m.Type != "forward_ready" || m.Error != "" {
		t.Fatal(m)
	}
	remote, err := service.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	_ = remote.SetDeadline(time.Now().Add(5 * time.Second))
	app, local := tcpPair(t)
	done := make(chan struct{})
	go func() { defer close(done); defer st.Close(); relayForward(context.Background(), local, st) }()
	payload := bytes.Repeat([]byte("binary\x00\xff"), 20000)
	go func() { _, _ = app.Write(payload); _ = app.CloseWrite() }()
	got, err := io.ReadAll(remote)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("remote request", len(got), err)
	}
	go func() { _, _ = remote.Write(payload); _ = remote.(*net.TCPConn).CloseWrite() }()
	got, err = io.ReadAll(app)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("remote response", len(got), err)
	}
	awaitForward(t, done)

	// Detach closes a separate, idle remote TCP connection and leaves the shell.
	idle, err := b.mux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	sendTest(t, idle, message{Type: "forward_connect", Port: service.Addr().(*net.TCPAddr).Port})
	if m := recv(t, idle); m.Error != "" {
		t.Fatal(m)
	}
	peer, err := service.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	// Exercise an actual daemon reverse listener before the same detach.
	reversePort := port
	if r := f.request(message{Action: "add", Reverse: true, Port: reversePort, RemoteHost: "127.0.0.1", RemotePort: service.Addr().(*net.TCPAddr).Port}); r.Error != "" {
		t.Fatal(r.Error)
	}
	reverseApp, err := net.DialTimeout("tcp4", loopback(reversePort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer reverseApp.Close()
	service.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	reversePeer, err := service.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer reversePeer.Close()
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	var one [1]byte
	if _, err = peer.Read(one[:]); err != io.EOF {
		t.Fatal("remote connection survived detach:", err)
	}
	reversePeer.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err = reversePeer.Read(one[:]); err != io.EOF {
		t.Fatal("reverse connection survived detach:", err)
	}
	// Listener teardown can finish just after the detached acknowledgement.
	deadline := time.Now().Add(3 * time.Second)
	for {
		ln, e := net.Listen("tcp4", loopback(reversePort))
		if e == nil {
			ln.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reverse listener survived detach:", e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.close()
	b.close()
	next, _ := e.attach("main")
	sendTest(t, next.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, next.tty, "exit")
}

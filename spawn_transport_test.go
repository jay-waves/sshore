package main

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xtaci/smux"
)

func TestFrontendMuxLargeStream(t *testing.T) {
	client, server := spawnNetworkMuxPair(t)
	send, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	receive, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := send.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := receive.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("sshore-mux-data"), 32*1024)
	done := make(chan error, 1)
	go func() { _, err := send.Write(payload); done <- err }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(receive, got); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("large mux stream changed data")
	}
}

func TestDirectSpawnOwnerAndDrain(t *testing.T) {
	t.Run("frontend disconnect preserves owner", func(t *testing.T) {
		func(t *testing.T) {
			frontend, relay := spawnNetworkMuxPair(t)
			ownerDone := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				relayDirectSpawn(relay, ownerDone, func() (net.Conn, error) { return nil, io.ErrClosedPipe })
			}()
			frontend.Close()
			time.Sleep(10 * time.Millisecond)
			select {
			case <-done:
				t.Fatal("frontend disconnect ended spawn owner")
			default:
			}
			close(ownerDone)
			<-done
		}(t)
	})
	t.Run("owner end rejects new endpoints and drains tail", func(t *testing.T) {
		func(t *testing.T) {
			frontend, relay := spawnNetworkMuxPair(t)
			ownerDone := make(chan struct{})
			done := make(chan struct{})
			peers := make(chan net.Conn, 1)
			var opened atomic.Int32
			go func() {
				defer close(done)
				relayDirectSpawn(relay, ownerDone, func() (net.Conn, error) {
					opened.Add(1)
					a, b := net.Pipe()
					peers <- b
					return a, nil
				})
			}()
			stream, err := frontend.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			peer := <-peers
			defer peer.Close()
			close(ownerDone)
			time.Sleep(10 * time.Millisecond)
			next, err := frontend.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			_ = next.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := next.Read(make([]byte, 1)); err == nil {
				t.Fatal("new endpoint accepted after owner ended")
			}
			next.Close()
			if opened.Load() != 1 {
				t.Fatal("dialed after owner ended")
			}
			go func() { peer.Write([]byte("exit tail")); peer.Close() }()
			tail, err := io.ReadAll(stream)
			if err != nil || string(tail) != "exit tail" {
				t.Fatalf("lost tail: %q %v", tail, err)
			}
			<-done
		}(t)
	})
}

// SSH forwarded channels do not implement SetReadDeadline.
type noReadDeadline struct{ net.Conn }

func (noReadDeadline) SetReadDeadline(time.Time) error { return nil }
func TestDirectSpawnMissingOwnerPulse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := net.Pipe()
		defer b.Close()
		go func() { _ = writeMessage(b, message{Type: "activity_ready"}) }()
		stop, done, err := acceptActivityLease(t.Context(), noReadDeadline{a})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		time.Sleep(daemonHeartbeatTimeout + time.Second)
		select {
		case <-done:
		default:
			t.Fatal("silent owner without deadline support remained connected")
		}
	})
}

// smux pools result channels globally, so its sessions cannot cross synctest
// bubbles. These lifecycle checks use real loopback TCP instead.
func spawnNetworkMuxPair(t *testing.T) (*smux.Session, *smux.Session) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.Accept()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	a, err := smux.Client(client, relayMuxConfig())
	if err != nil {
		client.Close()
		server.Close()
		t.Fatal(err)
	}
	b, err := smux.Server(server, relayMuxConfig())
	if err != nil {
		a.Close()
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

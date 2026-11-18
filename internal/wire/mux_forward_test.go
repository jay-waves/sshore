package wire

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func TestMuxForwardCloseJoinsActiveConnections(t *testing.T) {
	for _, cause := range []string{"close", "listener", "transport"} {
		t.Run(cause, func(t *testing.T) {
			service, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			a, b := net.Pipe()
			client, err := smux.Client(a, MuxConfig())
			if err != nil {
				t.Fatal(err)
			}
			server, err := smux.Server(b, MuxConfig())
			if err != nil {
				t.Fatal(err)
			}
			origin := StartMuxForward(client, listener, 0)
			remote := StartMuxForward(server, nil, service.Addr().(*net.TCPAddr).Port)
			defer origin.Close()
			defer remote.Close()
			incoming, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer incoming.Close()
			service.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
			outgoing, err := service.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer outgoing.Close()
			// Transfer data so both sides have installed their forwarding workers.
			written := make(chan error, 1)
			go func() { _, err := incoming.Write([]byte("x")); written <- err }()
			outgoing.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.ReadFull(outgoing, make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				switch cause {
				case "close":
					var wg sync.WaitGroup
					for range 4 {
						wg.Go(func() { origin.Close() })
					}
					wg.Wait()
				case "listener":
					listener.Close()
				case "transport":
					server.Close()
				}
				origin.Wait()
				remote.Wait()
				close(done)
			}()
			awaitTransport(t, done)
			for _, c := range []net.Conn{incoming, outgoing} {
				c.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := c.Read(make([]byte, 1)); err == nil {
					t.Fatal("connection survived completed shutdown")
				} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					t.Fatal("shutdown left an open connection")
				}
			}
			if c, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
				c.Close()
				t.Fatal("listener survived completed shutdown")
			}
		})
	}
}

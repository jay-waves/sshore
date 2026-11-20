package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	forwardio "github.com/jay-waves/sshore/internal/forward"
	cryptossh "golang.org/x/crypto/ssh"
)

type terminalEOFControl struct {
	net.Conn
	terminalEOF chan struct{}
}

func TestInputDrainTimeoutKeepsControlAvailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		control, peer := net.Pipe()
		terminal, terminalPeer := net.Pipe()
		defer peer.Close()
		defer terminalPeer.Close()
		a := newAttachment(control, terminal)
		defer a.Close()
		if err := a.DrainInput(1); err != nil {
			t.Fatal(err)
		}
		// No input_drained was sent; the next frame can still confirm detach.
		done := make(chan error, 1)
		go func() { done <- a.controlMessage(message{Type: "detached"}) }()
		m, err := readMessage(peer)
		if err != nil || m.Type != "detached" {
			t.Fatalf("control after timeout: %+v %v", m, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func (c terminalEOFControl) SetReadDeadline(at time.Time) error {
	err := c.Conn.SetReadDeadline(at)
	close(c.terminalEOF)
	return err
}

func TestTerminalEventsAfterDataEOF(t *testing.T) {
	for _, tc := range []struct {
		name         string
		reply        message
		wantErr      error
		keepDataOpen bool
	}{
		{name: "complete exit", reply: message{Type: "exit", TerminalOffset: 4, Code: 7}},
		{name: "truncated exit", reply: message{Type: "exit", TerminalOffset: 5}, wantErr: io.ErrUnexpectedEOF},
		{name: "missing exit bytes", reply: message{Type: "exit", TerminalOffset: 5}, wantErr: io.ErrUnexpectedEOF, keepDataOpen: true},
		{name: "detach", reply: message{Type: "detached"}},
		{name: "control lost", wantErr: io.EOF},
		{name: "control silent", wantErr: os.ErrDeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				control, peer := net.Pipe()
				defer control.Close()
				defer peer.Close()
				terminal, data := net.Pipe()
				defer terminal.Close()
				defer data.Close()
				eof := make(chan struct{})
				events := make(chan clientEvent, 2)
				readAttachmentEvents(t.Context(), terminalEOFControl{control, eof}, terminal, func(e clientEvent) { events <- e }, nil)
				if _, err := data.Write([]byte("tail")); err != nil {
					t.Fatal(err)
				}
				if !tc.keepDataOpen {
					_ = data.Close()
					<-eof
				}
				switch {
				case tc.reply.Type != "":
					if err := writeMessage(peer, tc.reply); err != nil {
						t.Fatal(err)
					}
				case tc.wantErr == io.EOF:
					_ = peer.Close()
				}
				if e := <-events; string(e.output) != "tail" || e.err != nil {
					t.Fatalf("expected exit tail first, got %+v", e)
				}
				e := <-events
				if !errors.Is(e.err, tc.wantErr) || tc.wantErr == nil && (e.m.Type != tc.reply.Type || e.m.Code != tc.reply.Code) {
					t.Fatalf("unexpected final event: %+v", e)
				}
			})
		})
	}
}

func TestRoutedTerminalEndpointKeepsSelectedInstance(t *testing.T) {
	frontend, parent := muxPair(t)
	targetInstance := strings.Repeat("b", 32)
	parentInstance := strings.Repeat("a", 32)
	routeToken := strings.Repeat("c", 64)
	var endpoints terminalEndpoints
	serve := func(action, instance string) error {
		c, err := parent.AcceptStream()
		if err != nil {
			return err
		}
		defer c.Close()
		request, err := readMessage(c)
		if err != nil || request.Type != "spawn_route_connect" || request.Token != routeToken || request.Instance != parentInstance {
			return fmt.Errorf("unexpected parent route: %+v %v", request, err)
		}
		if err := writeMessage(c, message{Type: "route_connected", SoftwareVersion: version}); err != nil {
			return err
		}
		request, err = readMessage(c)
		if err != nil || request.Type != "shell_connect" || request.Action != action || request.Instance != instance {
			return fmt.Errorf("unexpected target selection: %+v %v", request, err)
		}
		if err := writeMessage(c, message{Type: "setup_ready", SoftwareVersion: version, Instance: targetInstance}); err != nil {
			return err
		}
		if action == "attach" {
			request, err := readMessage(c)
			if err != nil || request.Type != "terminal_open" {
				return fmt.Errorf("unexpected terminal request: %+v %v", request, err)
			}
			return endpoints.serve(c, request)
		}
		return nil
	}
	errs := make(chan error, 2)
	go func() {
		if err := serve("launch", ""); err != nil {
			errs <- err
			return
		}
		errs <- serve("attach", targetInstance)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	o := options{command: "launch", session: "target", routeToken: routeToken, routeTarget: true, routeParent: &options{
		session: parentInstance, instance: parentInstance,
		reusable: &clientTransport{instance: parentInstance, open: func(context.Context) (net.Conn, error) {
			return frontend.OpenStream()
		}},
	}}
	control, err := openClientTransportContext(ctx, &o, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	terminal, token, err := openTerminalEndpoint(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	endpoint, err := endpoints.take(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}

// The loopback SSH fixture forwards only into this test's in-memory daemon.
// It never opens account shells or uses real daemon state.
func terminalSSHFixture(t *testing.T, handle func(net.Conn)) func(context.Context) (net.Conn, error) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := cryptossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &cryptossh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, e := listener.Accept()
		if e != nil {
			return
		}
		defer raw.Close()
		server, channels, requests, e := cryptossh.NewServerConn(raw, config)
		if e != nil {
			return
		}
		defer server.Close()
		go cryptossh.DiscardRequests(requests)
		var workers sync.WaitGroup
		defer workers.Wait()
		for next := range channels {
			if next.ChannelType() != "direct-tcpip" {
				next.Reject(cryptossh.UnknownChannelType, "expected direct-tcpip")
				continue
			}
			channel, requests, e := next.Accept()
			if e != nil {
				continue
			}
			go cryptossh.DiscardRequests(requests)
			a, b := net.Pipe()
			workers.Go(func() { forwardio.Bridge(channel, a) })
			workers.Go(func() { defer b.Close(); handle(b) })
		}
	}()
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, channels, requests, err := cryptossh.NewClientConn(raw, listener.Addr().String(), &cryptossh.ClientConfig{User: "fixture", HostKeyCallback: cryptossh.FixedHostKey(signer.PublicKey())})
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	client := cryptossh.NewClient(conn, channels, requests)
	t.Cleanup(func() {
		client.Close()
		listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("SSH fixture did not stop")
		}
	})
	return func(ctx context.Context) (net.Conn, error) {
		return client.DialContext(ctx, "tcp", "127.0.0.1:12345")
	}
}

type endpointInput struct{ p *attachment }

func (i endpointInput) Write(b []byte) (int, error) {
	if err := i.p.Output(b); err != nil {
		return 0, err
	}
	return len(b), nil
}
func (endpointInput) Stopped() bool { return false }

type endpointClient struct {
	data    io.ReadWriteCloser
	control attachmentControl
}

func (c endpointClient) close() { c.data.Close(); c.control.Close() }

func TestIndependentTerminalSSHChannels(t *testing.T) {
	testTerminalEndpoints(t, terminalSSHFixture)
}

func TestDirectSpawnTerminalEndpoints(t *testing.T) {
	testTerminalEndpoints(t, func(t *testing.T, handle func(net.Conn)) func(context.Context) (net.Conn, error) {
		var channels atomic.Int32
		release := make(chan struct{})
		open := terminalSSHFixture(t, func(c net.Conn) {
			if channels.Add(1) != 1 {
				handle(c)
				return
			}
			if err := writeMessage(c, message{Type: "activity_ready"}); err != nil {
				return
			}
			if err := writeMessage(c, message{Type: "pulse"}); err != nil {
				return
			}
			if m, err := readMessage(c); err != nil || m.Type != "alive" {
				t.Errorf("owner heartbeat: %+v %v", m, err)
				return
			}
			<-release
		})
		owner, err := open(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		stop, ownerDone, err := acceptActivityLease(t.Context(), owner.(net.Conn))
		if err != nil {
			owner.Close()
			t.Fatal(err)
		}
		frontend, relay := muxPair(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			relayDirectSpawn(relay, ownerDone, func() (net.Conn, error) {
				c, err := open(t.Context())
				if err != nil {
					return nil, err
				}
				return c.(net.Conn), nil
			})
		}()
		t.Cleanup(func() {
			close(release)
			frontend.Close()
			owner.Close()
			stop()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("direct spawn relay did not stop")
			}
		})
		return func(context.Context) (net.Conn, error) { return frontend.OpenStream() }
	})
}

func testTerminalEndpoints(t *testing.T, fixture func(*testing.T, func(net.Conn)) func(context.Context) (net.Conn, error)) {
	t.Helper()
	var endpoints terminalEndpoints
	attachments := make(chan *attachment, 8)
	open := fixture(t, func(c net.Conn) {
		first, err := readMessage(c)
		if err != nil {
			return
		}
		if first.Type == "terminal_open" {
			_ = endpoints.serve(c, first)
			return
		}
		if first.Type != "attach_control" {
			return
		}
		if err := writeMessage(c, message{Type: "attach_control_ready", SoftwareVersion: version}); err != nil {
			return
		}
		terminal, _, err := acceptAttachment(c, &endpoints)
		if err != nil {
			return
		}
		p := newAttachment(c, terminal)
		defer p.Close()
		attachments <- p
		inputDone := make(chan struct{})
		go func() { defer close(inputDone); p.ReadInput(endpointInput{p}, func() bool { return true }) }()
		defer func() { p.Close(); <-inputDone }()
		if err := p.controlMessage(message{Type: "ready"}); err != nil {
			return
		}
		for {
			m, err := readMessage(c)
			if err != nil {
				return
			}
			switch m.Type {
			case "resize":
				_ = p.controlMessage(message{Type: "resized", Rows: m.Rows, Cols: m.Cols})
			case "input_barrier":
				if p.DrainInput(m.TerminalOffset) != nil {
					return
				}
			case "detach":
				_ = p.FinishDetach("persistent")
				return
			case "fixture_exit":
				_ = p.Output([]byte("tail"))
				p.FinishExit(7, "fixture exit")
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connect := func() (endpointClient, *attachment) {
		t.Helper()
		data, token, err := openTerminalEndpoint(ctx, options{reusable: &clientTransport{open: open}})
		if err != nil {
			t.Fatal(err)
		}
		c, err := open(ctx)
		if err != nil {
			data.Close()
			t.Fatal(err)
		}
		control := ownClientTransport(ctx, c, nil)
		if err := clientAttachment(control); err != nil {
			data.Close()
			control.Close()
			t.Fatal(err)
		}
		peer := endpointClient{data, control}
		t.Cleanup(peer.close)
		if err := streamMessage(control, message{Type: "attach", Token: token, Term: "xterm"}); err != nil {
			t.Fatal(err)
		}
		if m, err := readMessage(control); err != nil || m.Type != "ready" {
			t.Fatalf("ready: %+v %v", m, err)
		}
		if _, err := endpoints.take(token); err == nil {
			t.Fatal("terminal token reused")
		}
		return peer, <-attachments
	}
	first, server := connect()
	// Raw bytes are not encoded or framed, including NUL and invalid UTF-8.
	binary := []byte{0, 255, 27, 13, 10, 1, 2, 3}
	if _, err := first.data.Write(binary); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(binary))
	if _, err := io.ReadFull(first.data, got); err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("raw echo: %x %v", got, err)
	}
	if err := streamMessage(first.control, message{Type: "input_barrier", TerminalOffset: uint64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if m, err := readMessage(first.control); err != nil || m.Type != "input_drained" {
		t.Fatalf("drain: %+v %v", m, err)
	}
	// Fill one endpoint's receive window; control and sibling terminal remain usable.
	blocked := make(chan error, 1)
	go func() { blocked <- server.Output(make([]byte, 8*1024*1024)) }()
	select {
	case err := <-blocked:
		t.Fatalf("expected SSH backpressure: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := streamMessage(first.control, message{Type: "resize", Cols: 101, Rows: 37}); err != nil {
		t.Fatal(err)
	}
	_ = first.control.SetReadDeadline(time.Now().Add(time.Second))
	if m, err := readMessage(first.control); err != nil || m.Type != "resized" || m.Cols != 101 || m.Rows != 37 {
		t.Fatalf("resize behind stalled terminal: %+v %v", m, err)
	}
	_ = first.control.SetReadDeadline(time.Time{})
	second, _ := connect()
	if _, err := second.data.Write(binary); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(second.data, got); err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("sibling terminal: %x %v", got, err)
	}
	first.data.Close()
	select {
	case <-server.Done():
	case <-time.After(time.Second):
		t.Fatal("terminal loss did not release server attachment")
	}
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("close did not release terminal writer")
	}
	if err := streamMessage(second.control, message{Type: "fixture_exit"}); err != nil {
		t.Fatal(err)
	}
	// Deliberately consume control first: the byte offset identifies output still to drain.
	m, err := readMessage(second.control)
	if err != nil || m.Type != "exit" || m.Code != 7 || m.TerminalOffset != uint64(len(binary)+4) {
		t.Fatalf("exit: %+v %v", m, err)
	}
	tail := make([]byte, 4)
	if _, err := io.ReadFull(second.data, tail); err != nil || string(tail) != "tail" {
		t.Fatalf("exit tail: %q %v", tail, err)
	}
	second.close()
	// Attachment shutdown must leave the authenticated SSH connection reusable.
	third, _ := connect()
	if err := streamMessage(third.control, message{Type: "detach"}); err != nil {
		t.Fatal(err)
	}
	m, err = readMessage(third.control)
	if err != nil || m.Type != "detached" || m.Name != "persistent" {
		t.Fatalf("detach: %+v %v", m, err)
	}
	if err := streamMessage(third.control, message{Type: "bye"}); err != nil {
		t.Fatal(err)
	}
	third.close()
	fourth, _ := connect()
	if _, err := fourth.data.Write([]byte("reconnected")); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, len("reconnected"))
	if _, err := io.ReadFull(fourth.data, got); err != nil || string(got) != "reconnected" {
		t.Fatal(fmt.Sprintf("reconnect: %q %v", got, err))
	}
}

func TestTerminalEndpointExpiryAndScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var owner, other terminalEndpoints
		server, client := net.Pipe()
		defer client.Close()
		done := make(chan error, 1)
		go func() { done <- owner.serve(server, message{SoftwareVersion: version}) }()
		ready, err := readMessage(client)
		if err != nil || ready.Type != "terminal_ready" {
			t.Fatalf("endpoint: %+v %v", ready, err)
		}
		if _, err := other.take(ready.Token); err == nil {
			t.Fatal("another shell claimed endpoint")
		}
		time.Sleep(6 * time.Second)
		if _, err := owner.take(ready.Token); err == nil {
			t.Fatal("expired token accepted")
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, err := client.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("expired endpoint remains open: %v", err)
		}
	})
}

func TestDetachHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		if _, err := expectDetachMessage(server, "detach"); err != nil {
			done <- err
			return
		}
		if err := writeMessage(server, message{Type: "detached", Name: "work"}); err != nil {
			done <- err
			return
		}
		_, err := expectDetachMessage(server, "bye")
		done <- err
	}()
	confirmed := ""
	if err := detachAttachment(client, func(name string) { confirmed = name }); err != nil {
		t.Fatal(err)
	}
	if confirmed != "work" {
		t.Fatal("lost confirmed name")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestApprovalAcknowledgesHelperBeforeClient(t *testing.T) {
	for _, helperDisconnects := range []bool{false, true} {
		t.Run(fmt.Sprintf("helper_disconnects_%t", helperDisconnects), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				control, client := net.Pipe()
				data, terminal := net.Pipe()
				helperServer, helper := net.Pipe()
				p := newAttachment(control, data)
				defer p.Close()
				defer client.Close()
				defer terminal.Close()
				defer helperServer.Close()
				defer helper.Close()
				p.approvals.enable()
				done := make(chan error, 1)
				go func() { done <- p.requestApproval(helperServer, message{Type: "approval_request"}) }()
				request, err := readMessage(client)
				if err != nil || request.Type != "approval_request" || request.Token == "" {
					t.Fatalf("approval request: %+v %v", request, err)
				}
				p.approvals.reply(message{Type: "approval_handoff", Token: "another request"})
				replies := make(chan message, 1)
				go func() { m, _ := readMessage(client); replies <- m }()
				synctest.Wait()
				select {
				case reply := <-replies:
					t.Fatalf("unrelated token completed the request: %+v", reply)
				default:
				}
				p.approvals.reply(message{Type: "approval_handoff", Token: request.Token})
				synctest.Wait()
				select {
				case reply := <-replies:
					t.Fatalf("client replied before helper: %+v", reply)
				default:
				}
				wantType := "approval_ack"
				if helperDisconnects {
					helper.Close()
					wantType = "approval_cancelled"
				} else if reply, err := readMessage(helper); err != nil || reply.Type != "approval_handoff" || reply.SoftwareVersion != version {
					t.Fatalf("helper reply: %+v %v", reply, err)
				} else {
					synctest.Wait()
					select {
					case reply := <-replies:
						t.Fatalf("client replied before remote helper closed: %+v", reply)
					default:
					}
					helper.Close()
				}
				if reply := <-replies; reply.Type != wantType || reply.Token != request.Token {
					t.Fatalf("client reply: %+v", reply)
				}
				if err := <-done; (err != nil) != helperDisconnects {
					t.Fatalf("approval result: %v", err)
				}
				if p.approvals.response != nil {
					t.Fatal("approval slot leaked")
				}
			})
		})
	}
}

func TestApprovalSlotsRequireReadyControl(t *testing.T) {
	var exchanges approvalExchanges
	if _, err := exchanges.open("not ready"); err == nil {
		t.Fatal("opened an approval before startup completed")
	}
	exchanges.enable()
	if _, err := exchanges.open("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := exchanges.open("concurrent"); err == nil {
		t.Fatal("opened a second approval")
	}
	exchanges.remove("first")
	if _, err := exchanges.open("available"); err != nil {
		t.Fatal(err)
	}
	exchanges.disable()
	exchanges.remove("available")
	if _, err := exchanges.open("detaching"); err == nil {
		t.Fatal("opened an approval during detach")
	}
}

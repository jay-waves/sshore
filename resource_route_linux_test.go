//go:build linux

package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/wire"
	"github.com/xtaci/smux"
)

func routedResourceFixture(t *testing.T) (*remoteShell, message) {
	t.Helper()
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", t.TempDir())
	s := &remoteShell{instance: "resource-instance", name: "source", shellTerminal: shellTerminal{process: processIdentity{PID: os.Getpid()}}, done: make(chan struct{})}
	return s, message{Type: "route_mount", SoftwareVersion: version, Instance: s.instance, Name: s.name, PID: os.Getpid(), ApprovalID: strings.Repeat("a", 32)}
}

func TestRoutedResourceRejectsChangedSource(t *testing.T) {
	for _, kind := range []string{"route_mount", "route_forward"} {
		for _, fault := range []string{"version", "instance", "pid", "approval", "disabled"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				s, request := routedResourceFixture(t)
				request.Type = kind
				if kind == "route_forward" {
					request.Operations = []operation{{Kind: "forward", Port: 12345, TargetPort: 23456}}
				}
				switch fault {
				case "version":
					request.SoftwareVersion = "0.0.1"
				case "instance":
					request.Instance = "replacement"
				case "pid":
					request.PID++
				case "approval":
					request.ApprovalID = "bad"
				case "disabled":
					t.Setenv("SSHORE_DISABLED", "1")
				}
				client, server := net.Pipe()
				defer client.Close()
				done := make(chan struct{})
				go func() { defer close(done); defer server.Close(); s.serveRoutedResource(server, request) }()
				client.SetDeadline(time.Now().Add(3 * time.Second))
				reply, err := readMessage(client)
				if err != nil || reply.Type != "error" {
					t.Fatalf("reply=%+v err=%v", reply, err)
				}
				<-done
			})
		}
	}
}

func TestRoutedForwardEndpointsAndLifetime(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "origin-listener"
		if reverse {
			name = "remote-listener"
		}
		t.Run(name, func(t *testing.T) {
			s, request := routedResourceFixture(t)
			service, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				for {
					c, e := service.Accept()
					if e != nil {
						return
					}
					accepted <- c
					go func(c net.Conn) {
						defer c.Close()
						data, err := io.ReadAll(c)
						if err == nil {
							_, _ = c.Write(data)
						}
					}(c)
				}
			}()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			target := service.Addr().(*net.TCPAddr).Port
			if reverse {
				listener.Close()
				listener = nil
			} else {
				defer listener.Close()
			}
			request.Type = "route_forward"
			request.Operations = []operation{{Kind: "forward", Port: port, TargetPort: target, Reverse: reverse}}
			client, server := net.Pipe()
			defer client.Close()
			done := make(chan struct{})
			go func() { defer close(done); defer server.Close(); s.serveRoutedResource(server, request) }()
			client.SetDeadline(time.Now().Add(5 * time.Second))
			reply, err := readMessage(client)
			if err != nil || reply.Type != "resource_ready" {
				t.Fatalf("reply=%+v err=%v", reply, err)
			}
			mx, err := smux.Client(client, wire.MuxConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer mx.Close()
			control, err := mx.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			marker, err := bufio.NewReader(control).ReadString('\n')
			if err != nil || marker != forwardReadyMarker+"\n" {
				t.Fatalf("marker=%q err=%v", marker, err)
			}
			forward := wire.StartMuxForward(mx, listener, target)
			defer forward.Close()
			// Approval establishes an independent resource, even after its source exits.
			close(s.done)
			c, err := net.DialTimeout("tcp", loopbackPort(port), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err = c.Write([]byte("routed")); err != nil {
				t.Fatal(err)
			}
			if err = c.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			b := make([]byte, 6)
			if _, err = io.ReadFull(c, b); err != nil || string(b) != "routed" {
				t.Fatalf("echo=%q err=%v", b, err)
			}
			peer := <-accepted
			defer peer.Close()
			idle, err := net.DialTimeout("tcp", loopbackPort(port), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer idle.Close()
			idle.SetDeadline(time.Now().Add(3 * time.Second))
			select {
			case peer := <-accepted:
				defer peer.Close()
			case <-time.After(3 * time.Second):
				t.Fatal("idle connection did not arrive")
			}
			// Losing the resource transport still closes active data connections.
			mx.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("resource outlived its transport")
			}
			if _, err = idle.Read(b); err == nil {
				t.Fatal("data connection survived route loss")
			}
		})
	}
}

func TestRoutedMountPreservesAgentApprovalGate(t *testing.T) {
	for _, fault := range []string{"version", "approval", "instance"} {
		t.Run(fault, func(t *testing.T) {
			s, request := routedResourceFixture(t)
			client, server := net.Pipe()
			defer client.Close()
			done := make(chan struct{})
			go func() { defer close(done); defer server.Close(); s.serveRoutedResource(server, request) }()
			client.SetDeadline(time.Now().Add(3 * time.Second))
			reply, err := readMessage(client)
			if err != nil || reply.Type != "resource_ready" {
				t.Fatalf("reply=%+v err=%v", reply, err)
			}
			release, id, instance := version, request.ApprovalID, request.Instance
			if fault == "version" {
				release = "0.0.1"
			} else if fault == "approval" {
				id = strings.Repeat("b", 32)
			} else {
				instance = "replacement"
			}
			metadata := map[string]any{"type": "mount_mux", "software_version": release, "mount_approval": map[string]any{"Session": request.Name, "PID": request.PID, "ID": id, "Instance": instance}}
			if err = wire.WriteMessage(client, "mount_mux", nil, metadata); err != nil {
				t.Fatal(err)
			}
			reply, err = readMessage(client)
			if err != nil || reply.Type != "error" {
				t.Fatalf("reply=%+v err=%v", reply, err)
			}
			if fault != "version" && !strings.Contains(reply.Error, "approval identity changed") {
				t.Fatal(reply.Error)
			}
			<-done
		})
	}
}

// A failed resource handshake must release the listener reserved before approval.
func TestRoutedForwardStartupCleanup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	_, err = startRoutedForward(context.Background(), forwardConfig{Port: port, Target: 12345, Connection: &recentConnection{}})
	if err == nil {
		t.Fatal("invalid resource route accepted")
	}
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("failed startup retained its listener: %v", err)
	}
	listener.Close()
}

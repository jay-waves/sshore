// Package sshtest serves an isolated SSH endpoint for CLI workflow tests.
package sshtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"sync"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
)

// Options selects only the remote behavior needed by a test. An omitted exec
// handler rejects session commands instead of starting a subprocess.
type Options struct {
	Exec          func(context.Context, string, cryptossh.Channel)
	GlobalRequest func(*cryptossh.Request)
	AllowForward  bool
	ConfigPrelude string // Optional shell statements to observe calls to the ssh -G shim.
}

// Start creates an authenticated loopback endpoint and returns its ssh -G shim.
func Start(t testing.TB, options Options) string {
	t.Helper()
	dir := t.TempDir()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := cryptossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var connections []net.Conn
	var closing bool
	var workers sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	track := func(c net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if closing {
			c.Close()
			return false
		}
		connections = append(connections, c)
		return true
	}
	sc := &cryptossh.ServerConfig{PublicKeyCallback: func(_ cryptossh.ConnMetadata, key cryptossh.PublicKey) (*cryptossh.Permissions, error) {
		if string(key.Marshal()) != string(signer.PublicKey().Marshal()) {
			return nil, fmt.Errorf("wrong fixture key")
		}
		return nil, nil
	}}
	sc.AddHostKey(signer)
	workers.Go(func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			if !track(raw) {
				return
			}
			workers.Go(func() {
				defer raw.Close()
				client, channels, requests, err := cryptossh.NewServerConn(raw, sc)
				if err != nil {
					return
				}
				defer client.Close()
				go func() {
					for request := range requests {
						if options.GlobalRequest != nil {
							options.GlobalRequest(request)
						} else {
							request.Reply(false, nil)
						}
					}
				}()
				for channel := range channels {
					if channel.ChannelType() == "direct-tcpip" {
						if !options.AllowForward {
							channel.Reject(cryptossh.Prohibited, "forwarding not enabled for fixture")
							continue
						}
						workers.Go(func() { serveForward(ctx, channel, track) })
						continue
					}
					if channel.ChannelType() != "session" {
						channel.Reject(cryptossh.UnknownChannelType, "workflow fixture")
						continue
					}
					stream, req, err := channel.Accept()
					if err != nil {
						continue
					}
					workers.Go(func() {
						defer stream.Close()
						for r := range req {
							if r.Type != "exec" || options.Exec == nil {
								r.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							if cryptossh.Unmarshal(r.Payload, &payload) != nil {
								return
							}
							r.Reply(true, nil)
							options.Exec(ctx, payload.Command, stream)
							return
						}
					})
				}
			})
		}
	})
	t.Cleanup(func() {
		cancel()
		listener.Close()
		mu.Lock()
		closing = true
		for _, c := range connections {
			c.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return writeConfig(t, dir, listener.Addr().String(), private, signer.PublicKey(), options.ConfigPrelude)
}

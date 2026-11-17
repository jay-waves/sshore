// Package sshtest serves an isolated SSH endpoint for CLI workflow tests.
package sshtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Wrap retains a workflow's remote subprocess fixture, replacing its fake SSH
// executable with a real authenticated channel and an ssh -G configuration tool.
func Wrap(t testing.TB, script string, env []string, globalRequest ...func(*cryptossh.Request)) string {
	t.Helper()
	dir := t.TempDir()
	if env == nil {
		env = os.Environ()
	}
	env = append([]string(nil), env...)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := cryptossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	al, err := net.Listen("unix", filepath.Join(dir, "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var connections []net.Conn
	var workers sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	track := func(c net.Conn) { mu.Lock(); connections = append(connections, c); mu.Unlock() }
	workers.Add(2)
	go func() {
		defer workers.Done()
		for {
			c, err := al.Accept()
			if err != nil {
				return
			}
			track(c)
			workers.Add(1)
			go func() { defer workers.Done(); defer c.Close(); agent.ServeAgent(ring, c) }()
		}
	}()
	sc := &cryptossh.ServerConfig{PublicKeyCallback: func(_ cryptossh.ConnMetadata, key cryptossh.PublicKey) (*cryptossh.Permissions, error) {
		if string(key.Marshal()) != string(signer.PublicKey().Marshal()) {
			return nil, fmt.Errorf("wrong fixture key")
		}
		return nil, nil
	}}
	sc.AddHostKey(signer)
	go func() {
		defer workers.Done()
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			track(raw)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer raw.Close()
				client, channels, requests, err := cryptossh.NewServerConn(raw, sc)
				if err != nil {
					return
				}
				defer client.Close()
				go func() {
					for request := range requests {
						if len(globalRequest) > 0 {
							globalRequest[0](request)
						} else {
							request.Reply(false, nil)
						}
					}
				}()
				for channel := range channels {
					if channel.ChannelType() == "direct-tcpip" {
						var request struct {
							Host       string
							Port       uint32
							OriginHost string
							OriginPort uint32
						}
						if cryptossh.Unmarshal(channel.ExtraData(), &request) != nil {
							channel.Reject(cryptossh.ConnectionFailed, "invalid socket request")
							continue
						}
						remote, err := net.Dial("tcp", net.JoinHostPort(request.Host, fmt.Sprint(request.Port)))
						if err != nil {
							channel.Reject(cryptossh.ConnectionFailed, err.Error())
							continue
						}
						track(remote)
						stream, requests, err := channel.Accept()
						if err != nil {
							remote.Close()
							continue
						}
						workers.Add(1)
						go func() {
							defer workers.Done()
							defer remote.Close()
							defer stream.Close()
							go cryptossh.DiscardRequests(requests)
							done := make(chan struct{})
							go func() { io.Copy(remote, stream); remote.Close(); close(done) }()
							io.Copy(stream, remote)
							stream.Close()
							<-done
						}()
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
					workers.Add(1)
					go func() {
						defer workers.Done()
						defer stream.Close()
						for r := range req {
							if r.Type != "exec" {
								r.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							if cryptossh.Unmarshal(r.Payload, &payload) != nil {
								return
							}
							r.Reply(true, nil)
							cmd := exec.CommandContext(ctx, script, "--", "fixture", payload.Command)
							cmd.Dir = filepath.Dir(script)
							cmd.Env = append(append([]string{}, env...), "SSHORE_CONFIRM_ADDR=", "SSHORE_CONFIRM_TOKEN=")
							// os/exec owns output copies; input copy is interrupted by channel closure.
							stdin, err := cmd.StdinPipe()
							if err != nil {
								return
							}
							cmd.Stdout, cmd.Stderr = stream, stream.Stderr()
							if err = cmd.Start(); err != nil {
								return
							}
							copied := make(chan struct{})
							go func() { io.Copy(stdin, stream); stdin.Close(); close(copied) }()
							err = cmd.Wait()
							status := 0
							if exit, ok := err.(*exec.ExitError); ok {
								status = exit.ExitCode()
							} else if err != nil {
								status = 255
							}
							stream.SendRequest("exit-status", false, cryptossh.Marshal(struct{ Status uint32 }{uint32(status)}))
							stream.Close()
							<-copied
							return
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		al.Close()
		mu.Lock()
		for _, c := range connections {
			c.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte("[127.0.0.1]:"+port+" "+string(cryptossh.MarshalAuthorizedKey(signer.PublicKey()))), 0600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "identity")
	os.WriteFile(keyPath+".pub", cryptossh.MarshalAuthorizedKey(signer.PublicKey()), 0600)
	output := fmt.Sprintf("hostname 127.0.0.1\nuser fixture\nport %s\nidentityagent %s\nidentityfile %s\nidentitiesonly yes\nstricthostkeychecking yes\nuserknownhostsfile %s\n", port, filepath.Join(dir, "agent.sock"), keyPath, known)
	shim := filepath.Join(dir, "ssh")
	body := "#!/bin/sh\n[ \"$1\" = -G ] || exit 90\nif [ -n \"$SSHORE_TEST_CONFIG_OWNER\" ]; then printf '%s\\n%s\\n' \"$PPID\" \"$PWD\" > \"$SSHORE_TEST_CONFIG_OWNER\"; fi\nif [ -n \"$SSHORE_TEST_CONFIG_CONTEXT\" ]; then printf '%s\\n%s\\n' \"$PWD\" \"$HANDOFF_VALUE\" > \"$SSHORE_TEST_CONFIG_CONTEXT\"; fi\nprintf '%s' " + quote(output) + "\n"
	if err := os.WriteFile(shim, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return shim
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

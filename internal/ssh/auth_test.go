package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

func TestInteractiveLogin(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH configuration resolver unavailable")
	}
	for _, mode := range []string{"password", "keyboard-interactive", "excluded", "batch", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			_, private, _ := ed25519.GenerateKey(rand.Reader)
			host, _ := cryptossh.NewSignerFromKey(private)
			server := &cryptossh.ServerConfig{PublicKeyCallback: func(cryptossh.ConnMetadata, cryptossh.PublicKey) (*cryptossh.Permissions, error) {
				return nil, fmt.Errorf("key rejected")
			}}
			if mode == "keyboard-interactive" {
				server.KeyboardInteractiveCallback = func(_ cryptossh.ConnMetadata, challenge cryptossh.KeyboardInteractiveChallenge) (*cryptossh.Permissions, error) {
					answers, err := challenge("Login", "Enter code", []string{"Code: ", "Name: "}, []bool{false, true})
					if err != nil {
						return nil, err
					}
					if len(answers) != 2 || answers[0] != "123456" || answers[1] != "alice" {
						return nil, fmt.Errorf("wrong answers")
					}
					return nil, nil
				}
			} else {
				server.PasswordCallback = func(_ cryptossh.ConnMetadata, secret []byte) (*cryptossh.Permissions, error) {
					if mode == "rejected" || string(secret) != "correct" {
						return nil, fmt.Errorf("wrong password")
					}
					return nil, nil
				}
			}
			server.AddHostKey(host)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				raw, e := listener.Accept()
				if e != nil {
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(5 * time.Second))
				conn, channels, requests, e := cryptossh.NewServerConn(raw, server)
				if e != nil {
					return
				}
				defer conn.Close()
				go cryptossh.DiscardRequests(requests)
				for ch := range channels {
					ch.Reject(cryptossh.Prohibited, "test")
				}
			}()
			port := listener.Addr().(*net.TCPAddr).Port
			calls := 0
			options := Options{Host: "127.0.0.1", Args: []string{"-F", "none", "-p", fmt.Sprint(port), "-l", "alice", "-o", "IdentityAgent=none", "-o", "UserKnownHostsFile=none", "-o", "GlobalKnownHostsFile=none", "-o", "StrictHostKeyChecking=yes"}}
			// Trust only this ephemeral test server through a temporary known_hosts file.
			known := t.TempDir() + "/known_hosts"
			if err := os.WriteFile(known, []byte(fmt.Sprintf("[127.0.0.1]:%d %s", port, cryptossh.MarshalAuthorizedKey(host.PublicKey()))), 0600); err != nil {
				t.Fatal(err)
			}
			options.Args[9] = "UserKnownHostsFile=" + known
			options.Secret = func(_ context.Context, prompt string) ([]byte, error) {
				calls++
				if mode == "keyboard-interactive" {
					var c Challenge
					if err := json.Unmarshal([]byte(strings.TrimPrefix(prompt, "\x00challenge:")), &c); err != nil {
						t.Fatal(err)
					}
					if c.Echo {
						return []byte("alice"), nil
					}
					return []byte("123456"), nil
				}
				return []byte("correct"), nil
			}
			if mode == "excluded" {
				options.Args = append(options.Args, "-o", "PreferredAuthentications=publickey")
			}
			if mode == "batch" {
				options.Args = append(options.Args, "-o", "BatchMode=yes")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Dial(ctx, options)
			if mode == "rejected" {
				if err == nil {
					client.Close()
					t.Fatal("rejected password accepted")
				}
				if !strings.Contains(err.Error(), "server rejected SSH authentication") || strings.Contains(err.Error(), "IdentityFile") {
					t.Fatalf("misleading diagnostic: %v", err)
				}
				if calls != 3 {
					t.Fatalf("prompts %d", calls)
				}
			} else if mode == "excluded" || mode == "batch" {
				if err == nil {
					client.Close()
					t.Fatal("unexpected authentication")
				}
				if calls != 0 {
					t.Fatal("unexpected prompt")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				client.Close()
				want := 1
				if mode == "keyboard-interactive" {
					want = 2
				}
				if calls != want {
					t.Fatalf("prompts %d", calls)
				}
			}
			listener.Close()
			<-done
		})
	}
}

func TestMissingIdentityDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-key")
	c := &connectionConfig{values: map[string][]string{"identityagent": {"none"}, "identityfile": {path}}}
	_, _, err := c.authentication(context.Background())
	if err == nil || !strings.Contains(err.Error(), path) || strings.ContainsAny(err.Error(), "\r\n") {
		t.Fatalf("missing identity diagnostic: %v", err)
	}
}

func TestPrivateFileDoesNotAuthenticateWithoutAgent(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	block, err := cryptossh.MarshalPrivateKey(key, "test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	c := &connectionConfig{values: map[string][]string{"identityagent": {"none"}, "identityfile": {path}}}
	if _, _, err := c.authentication(context.Background()); err == nil {
		t.Fatal("private key loaded without agent")
	}
	signer, _ := cryptossh.NewSignerFromKey(key)
	if err := os.WriteFile(path+".pub", cryptossh.MarshalAuthorizedKey(signer.PublicKey()), 0600); err != nil {
		t.Fatal(err)
	}
	allowed, _, err := c.publicIdentities()
	if err != nil || !allowed[string(signer.PublicKey().Marshal())] {
		t.Fatalf("public identity selection: %v", err)
	}
	if _, _, err := c.authentication(context.Background()); err == nil {
		t.Fatal("public sidecar bypassed agent")
	}
}

func TestAuthenticationBudgetPausesJumpChain(t *testing.T) {
	g := &authenticationBudgets{entries: make(map[*authenticationBudget]bool)}
	outer, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner, stop := context.WithCancel(outer)
	defer stop()
	release := g.add(30*time.Millisecond, cancel)
	defer release()
	release2 := g.add(30*time.Millisecond, stop)
	defer release2()
	resume := g.pause()
	time.Sleep(70 * time.Millisecond)
	if outer.Err() != nil || inner.Err() != nil {
		t.Fatal("prompt consumed connection timeout")
	}
	resume()
	select {
	case <-inner.Done():
	case <-time.After(time.Second):
		t.Fatal("network timeout did not resume")
	}
}

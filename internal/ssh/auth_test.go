package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

type testGSSAPIServer struct{}

func (testGSSAPIServer) AcceptSecContext([]byte) ([]byte, string, bool, error) {
	return nil, "", false, errors.New("unexpected GSSAPI authentication attempt")
}

func (testGSSAPIServer) VerifyMIC([]byte, []byte) error {
	return errors.New("unexpected GSSAPI authentication attempt")
}

func (testGSSAPIServer) DeleteSecContext() error { return nil }

func TestInteractiveLogin(t *testing.T) {
	modes := []string{
		"password", "keyboard-interactive", "cancel-password", "cancel-keyboard-interactive",
		"excluded", "disabled-password", "restricted-public-key", "batch", "rejected",
		"file", "rejected-file", "fallback-file", "encrypted-file", "cancel-encrypted-file", "batch-encrypted-file",
		"gssapi-only-server", "gssapi-only-client",
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			cancelPrompt := strings.HasPrefix(mode, "cancel-")
			interactive := strings.HasSuffix(mode, "keyboard-interactive")
			identityFile := strings.HasSuffix(mode, "file")
			encrypted := strings.HasSuffix(mode, "encrypted-file")
			rejectedKey := false
			_, private, _ := ed25519.GenerateKey(rand.Reader)
			host, _ := cryptossh.NewSignerFromKey(private)
			server := &cryptossh.ServerConfig{PublicKeyCallback: func(cryptossh.ConnMetadata, cryptossh.PublicKey) (*cryptossh.Permissions, error) {
				return nil, fmt.Errorf("key rejected")
			}}
			if interactive {
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
					if mode == "rejected-file" && !rejectedKey {
						return nil, errors.New("password arrived before the public-key attempt")
					}
					if mode == "rejected" || string(secret) != "correct" {
						return nil, fmt.Errorf("wrong password")
					}
					return nil, nil
				}
			}
			server.GSSAPIWithMICConfig = &cryptossh.GSSAPIWithMICConfig{
				Server: testGSSAPIServer{},
				AllowLogin: func(cryptossh.ConnMetadata, string) (*cryptossh.Permissions, error) {
					return nil, errors.New("unexpected GSSAPI login")
				},
			}
			if mode == "gssapi-only-server" {
				server.PasswordCallback = nil
				server.PublicKeyCallback = nil
			}
			if mode == "gssapi-only-client" {
				server.NoClientAuth = true
			}
			server.AddHostKey(host)
			path := filepath.Join(t.TempDir(), "private key")
			var firstIdentity string
			if identityFile {
				_, key, _ := ed25519.GenerateKey(rand.Reader)
				signer, err := cryptossh.NewSignerFromKey(key)
				if err != nil {
					t.Fatal(err)
				}
				if mode != "rejected-file" {
					server.PasswordCallback = nil
				}
				var firstKey cryptossh.PublicKey
				if mode == "fallback-file" {
					_, initial, err := ed25519.GenerateKey(rand.Reader)
					if err != nil {
						t.Fatal(err)
					}
					firstKey, err = cryptossh.NewPublicKey(initial.Public())
					if err != nil {
						t.Fatal(err)
					}
					block, err := cryptossh.MarshalPrivateKeyWithPassphrase(initial, "test", []byte("different"))
					if err != nil {
						t.Fatal(err)
					}
					firstIdentity = filepath.Join(filepath.Dir(path), "first identity")
					if err := os.WriteFile(firstIdentity, pem.EncodeToMemory(block), 0600); err != nil {
						t.Fatal(err)
					}
				}
				server.PublicKeyCallback = func(_ cryptossh.ConnMetadata, offered cryptossh.PublicKey) (*cryptossh.Permissions, error) {
					if firstKey != nil && string(offered.Marshal()) == string(firstKey.Marshal()) {
						return nil, nil
					}
					if string(offered.Marshal()) != string(signer.PublicKey().Marshal()) {
						return nil, fmt.Errorf("wrong key")
					}
					if mode == "rejected-file" {
						rejectedKey = true
						return nil, errors.New("known key rejected")
					}
					return nil, nil
				}
				var block *pem.Block
				if encrypted {
					block, err = cryptossh.MarshalPrivateKeyWithPassphrase(key, "test", []byte("correct"))
				} else {
					block, err = cryptossh.MarshalPrivateKey(key, "test")
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
					t.Fatal(err)
				}
			}
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
			options := Options{Host: "127.0.0.1", Args: []string{"-F", "none", "-p", fmt.Sprint(port), "-l", "alice", "-o", "UserKnownHostsFile=none", "-o", "GlobalKnownHostsFile=none", "-o", "StrictHostKeyChecking=yes"}}
			// Trust only this ephemeral test server through a temporary known_hosts file.
			known := t.TempDir() + "/known_hosts"
			if err := os.WriteFile(known, []byte(fmt.Sprintf("[127.0.0.1]:%d %s", port, cryptossh.MarshalAuthorizedKey(host.PublicKey()))), 0600); err != nil {
				t.Fatal(err)
			}
			options.Args[7] = `UserKnownHostsFile="` + known + `"`
			options.Args = append(options.Args, "-o", "GSSAPIAuthentication=yes", "-o", "GSSAPIDelegateCredentials=yes", "-o", "HostbasedAuthentication=yes")
			if firstIdentity != "" {
				options.Args = append(options.Args, "-i", firstIdentity, "-o", "NumberOfPasswordPrompts=1")
			}
			if identityFile {
				options.Args = append(options.Args, "-i", path, "-o", "IdentitiesOnly=yes")
			}
			options.Secret = func(_ context.Context, prompt string) ([]byte, error) {
				calls++
				if encrypted && prompt != "Passphrase for "+path {
					t.Fatalf("wrong private key prompt: %q", prompt)
				}
				if cancelPrompt {
					return nil, context.Canceled
				}
				if interactive {
					var c Challenge
					if err := json.Unmarshal([]byte(strings.TrimPrefix(prompt, "\x00challenge:")), &c); err != nil {
						t.Fatal(err)
					}
					if c.Echo {
						return []byte("alice"), nil
					}
					return []byte("123456"), nil
				}
				if encrypted && calls == 1 {
					return []byte("wrong"), nil
				}
				return []byte("correct"), nil
			}
			switch mode {
			case "password":
				options.Args = append(options.Args, "-o", "PreferredAuthentications=gssapi-with-mic,hostbased,password")
			case "rejected-file":
				options.Args = append(options.Args, "-o", "PreferredAuthentications=gssapi-with-mic,publickey,password")
			case "disabled-password":
				options.Args = append(options.Args, "-o", "PasswordAuthentication=no")
			case "restricted-public-key":
				options.Args = append(options.Args, "-o", "PubkeyAcceptedAlgorithms=ssh-rsa", "-o", "PKCS11Provider=fixture-token")
			case "gssapi-only-client":
				options.Args = append(options.Args, "-o", "PreferredAuthentications=gssapi-with-mic")
			case "excluded":
				options.Args = append(options.Args, "-o", "PreferredAuthentications=publickey")
			}
			if strings.HasPrefix(mode, "batch") {
				options.Args = append(options.Args, "-o", "BatchMode=yes")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Dial(ctx, options)
			if cancelPrompt {
				if client != nil {
					client.Close()
				}
				if !errors.Is(err, context.Canceled) || calls != 1 {
					t.Fatalf("expected cancellation after one prompt, got %v after %d prompts", err, calls)
				}
			} else if mode == "gssapi-only-server" {
				if err == nil {
					client.Close()
					t.Fatal("unexpected authentication")
				}
				if !strings.Contains(err.Error(), "ssh: unable to authenticate") || calls != 0 {
					t.Fatalf("expected SSH authentication failure without prompts, got %v after %d prompts", err, calls)
				}
			} else if mode == "rejected" {
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
			} else if mode == "excluded" || mode == "disabled-password" || strings.HasPrefix(mode, "batch") {
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
				want := 1
				if mode == "file" || mode == "gssapi-only-client" {
					want = 0
				}
				if interactive || encrypted {
					want = 2
				}
				if calls != want {
					t.Fatalf("prompts %d", calls)
				}
				client.Close()
			}
			listener.Close()
			<-done
		})
	}
}

func TestMissingIdentityDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-key")
	c := &connectionConfig{values: map[string][]string{"identityfile": {path}}}
	_, err := c.publicKeySigners()
	if err == nil || !strings.Contains(err.Error(), path) || strings.ContainsAny(err.Error(), "\r\n") {
		t.Fatalf("missing identity diagnostic: %v", err)
	}
}

func TestAuthenticationBudgetPausesJumpChain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g authenticationBudgets
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
	})
}

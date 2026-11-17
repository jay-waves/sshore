package ssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestHostKeyConfiguredOrder(t *testing.T) {
	fixture := newConnectionFixture(t)
	cfg, err := resolveConfig(context.Background(), fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	configured := []string{cryptossh.KeyAlgoED25519, cryptossh.KeyAlgoRSASHA512, cryptossh.KeyAlgoRSASHA256}
	cfg.values["hostkeyalgorithms"] = []string{strings.Join(configured, ",")}
	_, trusted, err := cfg.hostKeys(context.Background())
	if err != nil || len(trusted) != 1 || trusted[0] != cryptossh.KeyAlgoED25519 {
		t.Fatalf("known host restriction changed: %v, %v", trusted, err)
	}
	delete(cfg.values, "userknownhostsfile")
	_, algorithms, err := cfg.hostKeys(context.Background())
	if err != nil || strings.Join(algorithms, ",") != strings.Join(configured, ",") {
		t.Fatalf("configuration order lost: %v, %v", algorithms, err)
	}
	cfg.values["hostkeyalgorithms"] = []string{cryptossh.KeyAlgoRSA}
	if _, _, err := cfg.hostKeys(context.Background()); err == nil {
		t.Fatal("legacy RSA/SHA-1 accepted")
	}
}

func TestNegotiatedAlgorithmDiagnostics(t *testing.T) {
	for _, level := range []string{"INFO", "DEBUG", "DEBUG3"} {
		t.Run(level, func(t *testing.T) {
			fixture := newConnectionFixture(t)
			config, err := os.ReadFile(fixture.options.Executable)
			if err != nil {
				t.Fatal(err)
			}
			config = bytes.Replace(config, []byte("hostname 127.0.0.1"), []byte("loglevel "+level+"\nhostname 127.0.0.1"), 1)
			if err := os.WriteFile(fixture.options.Executable, config, 0700); err != nil {
				t.Fatal(err)
			}
			var diagnostic bytes.Buffer
			fixture.options.Diagnostic = &diagnostic
			client, err := Dial(context.Background(), fixture.options)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if level == "INFO" {
				if diagnostic.Len() != 0 {
					t.Fatalf("normal connection produced diagnostics: %s", &diagnostic)
				}
				return
			}
			metadata := client.Conn.(cryptossh.AlgorithmsConnMetadata).Algorithms()
			for _, want := range []string{"kex=" + metadata.KeyExchange, "hostkey=" + metadata.HostKey, "send=" + metadata.Write.Cipher, "receive=" + metadata.Read.Cipher, "(AEAD)"} {
				if !strings.Contains(diagnostic.String(), want) {
					t.Fatalf("missing negotiated %q: %s", want, &diagnostic)
				}
			}
		})
	}
}

type connectionFixture struct {
	options Options
	address string
	key     cryptossh.PublicKey
}

func newConnectionFixture(t *testing.T, identities ...any) *connectionFixture {
	t.Helper()
	return newConnectionFixtureAt(t, "127.0.0.1:0", identities...)
}

func newConnectionFixtureAt(t *testing.T, listenAddress string, identities ...any) *connectionFixture {
	t.Helper()
	dir := t.TempDir()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var identity any = private
	if len(identities) > 0 {
		identity = identities[0]
	}
	added := agent.AddedKey{PrivateKey: identity}
	if value, ok := identity.(agent.AddedKey); ok {
		added = value
	}
	signer, err := cryptossh.NewSignerFromKey(added.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner := signer
	if added.Certificate != nil {
		signer, err = cryptossh.NewCertSigner(added.Certificate, signer)
		if err != nil {
			t.Fatal(err)
		}
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(added); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(dir, "agent.sock")
	al, err := net.Listen("unix", agentPath)
	if err != nil {
		t.Fatal(err)
	}
	allowNone := listenAddress == "none"
	allowPassword := listenAddress == "password"
	if allowNone || allowPassword {
		listenAddress = "127.0.0.1:0"
	}
	server, err := net.Listen("tcp", listenAddress)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var connections []net.Conn
	var workers sync.WaitGroup
	track := func(c net.Conn) { mu.Lock(); connections = append(connections, c); mu.Unlock() }
	done := make(chan struct{})
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
			go func() { defer workers.Done(); defer c.Close(); agent.ServeAgent(keyring, c) }()
		}
	}()
	sc := &cryptossh.ServerConfig{NoClientAuth: allowNone, PublicKeyCallback: func(_ cryptossh.ConnMetadata, key cryptossh.PublicKey) (*cryptossh.Permissions, error) {
		if string(key.Marshal()) != string(signer.PublicKey().Marshal()) {
			return nil, fmt.Errorf("unexpected identity")
		}
		return nil, nil
	}}
	if allowPassword {
		sc.PasswordCallback = func(_ cryptossh.ConnMetadata, password []byte) (*cryptossh.Permissions, error) {
			if string(password) != "correct" {
				return nil, fmt.Errorf("invalid password")
			}
			return nil, nil
		}
	}
	sc.AddHostKey(hostSigner)
	go func() {
		defer workers.Done()
		for {
			raw, err := server.Accept()
			if err != nil {
				return
			}
			track(raw)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer raw.Close()
				c, channels, requests, err := cryptossh.NewServerConn(raw, sc)
				if err != nil {
					return
				}
				defer c.Close()
				go cryptossh.DiscardRequests(requests)
				for ch := range channels {
					if ch.ChannelType() != "session" {
						ch.Reject(cryptossh.UnknownChannelType, "unsupported")
						continue
					}
					stream, req, err := ch.Accept()
					if err != nil {
						continue
					}
					workers.Add(1)
					go func() {
						defer workers.Done()
						defer stream.Close()
						for request := range req {
							if request.Type != "exec" {
								request.Reply(false, nil)
								continue
							}
							request.Reply(true, nil)
							var command struct{ Command string }
							cryptossh.Unmarshal(request.Payload, &command)
							if command.Command == "block" {
								<-done
								return
							}
							io.WriteString(stream, "result:"+command.Command)
							stream.SendRequest("exit-status", false, cryptossh.Marshal(struct{ Status uint32 }{0}))
							return
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		close(done)
		al.Close()
		server.Close()
		mu.Lock()
		for _, c := range connections {
			c.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	publicPath := filepath.Join(dir, "identity")
	os.WriteFile(publicPath+".pub", cryptossh.MarshalAuthorizedKey(signer.PublicKey()), 0600)
	knownPath := filepath.Join(dir, "known_hosts")
	address := server.Addr().String()
	host, port, _ := net.SplitHostPort(address)
	// Normalize non-default ports exactly as OpenSSH's known_hosts format.
	os.WriteFile(knownPath, []byte("["+host+"]:"+port+" "+string(cryptossh.MarshalAuthorizedKey(hostSigner.PublicKey()))), 0600)
	config := fmt.Sprintf("hostname %s\nuser fixture\nport %s\nidentityagent %s\nidentityfile %s\nidentitiesonly yes\nuserknownhostsfile %s\nstricthostkeychecking yes\n", host, port, agentPath, publicPath, knownPath)
	executable := filepath.Join(dir, "ssh-config")
	os.WriteFile(executable, []byte("#!/bin/sh\n[ \"$1\" = -G ] || exit 90\ncat <<'SSHORE_CONFIG'\n"+config+"SSHORE_CONFIG\n"), 0700)
	return &connectionFixture{options: Options{Host: "fixture", Command: "echo", Executable: executable}, address: address, key: signer.PublicKey()}
}
func TestNativeSSHSessionAndIsolation(t *testing.T) {
	a, b := newConnectionFixture(t), newConnectionFixture(t)
	var workers sync.WaitGroup
	for _, fixture := range []*connectionFixture{a, b} {
		fixture := fixture
		workers.Add(1)
		go func() {
			defer workers.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			process, err := Start(ctx, fixture.options, io.Discard)
			if err != nil {
				t.Error(err)
				return
			}
			defer process.Close()
			data, err := io.ReadAll(process)
			if err != nil || string(data) != "result:echo" {
				t.Errorf("output %q: %v", data, err)
			}
			if err := process.Wait(); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
}
func TestNativeSSHCancelHandshake(t *testing.T) {
	f := newConnectionFixture(t)
	listener, err := net.Listen("tcp", f.address)
	if err == nil {
		listener.Close()
		t.Fatal("fixture did not bind its endpoint")
	}
	config, err := os.ReadFile(f.options.Executable)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	received := make(chan net.Conn, 1)
	go func() {
		c, err := blocked.Accept()
		if err == nil {
			received <- c
		}
	}()
	oldPort := strings.Split(f.address, ":")[1]
	newPort := strings.Split(blocked.Addr().String(), ":")[1]
	os.WriteFile(f.options.Executable, []byte(strings.Replace(string(config), "port "+oldPort, "port "+newPort, 1)), 0700)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		c, err := Dial(ctx, f.options)
		if c != nil {
			c.Close()
		}
		done <- err
	}()
	select {
	case c := <-received:
		defer c.Close()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("handshake not started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt handshake")
	}
}
func TestNativeSSHRejectsChangedHostKey(t *testing.T) {
	f := newConnectionFixture(t)
	cfg, err := resolveConfig(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := cryptossh.NewSignerFromKey(private)
	callback, _, err := cfg.hostKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := callback(f.address, &net.TCPAddr{}, other.PublicKey()); err == nil {
		t.Fatal("changed host key accepted")
	}
}

func TestRSAAgentAuthentication(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newConnectionFixture(t, key)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
}

func TestCertificateAgentAuthentication(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := cryptossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &cryptossh.Certificate{Key: signer.PublicKey(), CertType: cryptossh.UserCert, ValidPrincipals: []string{"fixture"}, ValidBefore: cryptossh.CertTimeInfinity}
	if err := certificate.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	fixture := newConnectionFixture(t, agent.AddedKey{PrivateKey: key, Certificate: certificate})
	config, err := os.ReadFile(fixture.options.Executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.options.Executable, []byte(strings.Replace(string(config), "identityfile ", "certificatefile ", 1)), 0700); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(filepath.Dir(fixture.options.Executable), "identity")
	if err := os.Rename(identity+".pub", identity); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	cfg := &connectionConfig{values: map[string][]string{"pubkeyacceptedalgorithms": {cryptossh.CertAlgoED25519v01}}}
	if !cfg.acceptsSignature(certificate, cryptossh.KeyAlgoED25519) || cfg.acceptsSignature(signer.PublicKey(), cryptossh.KeyAlgoED25519) {
		t.Fatal("certificate algorithm policy lost")
	}
}

func TestBracketedIPv6Connection(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	probe.Close()
	fixture := newConnectionFixtureAt(t, "[::1]:0")
	config, err := os.ReadFile(fixture.options.Executable)
	if err != nil {
		t.Fatal(err)
	}
	config = bytes.Replace(config, []byte("hostname ::1"), []byte("hostname [::1]"), 1)
	if err = os.WriteFile(fixture.options.Executable, config, 0700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err = Run(context.Background(), fixture.options, nil, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if output.String() != "result:echo" {
		t.Fatalf("output=%q", &output)
	}
}

func TestServerAcceptedNoneAuthentication(t *testing.T) {
	f := newConnectionFixtureAt(t, "none")
	data, err := os.ReadFile(f.options.Executable)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-len("SSHORE_CONFIG\n")], []byte("identityagent none\npubkeyauthentication no\nSSHORE_CONFIG\n")...)
	// Replace the existing agent field, since ssh -G uses first-value semantics.
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "identityagent ") {
			lines[i] = "identityagent none"
		}
		if strings.HasPrefix(line, "identityfile ") {
			lines[i] = "identityfile /missing/identity"
		}
	}
	if err := os.WriteFile(f.options.Executable, []byte(strings.Join(lines, "\n")), 0700); err != nil {
		t.Fatal(err)
	}
	c, err := Dial(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	for i, line := range lines {
		if strings.HasPrefix(line, "userknownhostsfile ") {
			lines[i] = "userknownhostsfile /missing/known_hosts"
		}
	}
	if err := os.WriteFile(f.options.Executable, []byte(strings.Join(lines, "\n")), 0700); err != nil {
		t.Fatal(err)
	}
	if c, err := Dial(context.Background(), f.options); err == nil {
		c.Close()
		t.Fatal("none authentication bypassed host trust")
	}
}

func TestServerRejectsNoneWithoutIdentity(t *testing.T) {
	f := newConnectionFixture(t)
	data, err := os.ReadFile(f.options.Executable)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "identityagent ") {
			lines[i] = "identityagent none"
		}
		if strings.HasPrefix(line, "identityfile ") {
			lines[i] = "identityfile /missing/identity"
		}
	}
	if err := os.WriteFile(f.options.Executable, []byte(strings.Join(lines, "\n")), 0700); err != nil {
		t.Fatal(err)
	}
	c, err := Dial(context.Background(), f.options)
	if err == nil {
		c.Close()
		t.Fatal("authenticated without server permission or credentials")
	}
	if !strings.Contains(err.Error(), "no usable SSH public-key identity") {
		t.Fatal(err)
	}
}

func TestPasswordFallbackWithoutKeys(t *testing.T) {
	f := newConnectionFixtureAt(t, "password")
	data, err := os.ReadFile(f.options.Executable)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "identityagent ") {
			lines[i] = "identityagent none"
		}
		if strings.HasPrefix(line, "identityfile ") {
			lines[i] = "identityfile /missing/key"
		}
	}
	if err := os.WriteFile(f.options.Executable, []byte(strings.Join(lines, "\n")), 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.options.Secret = func(_ context.Context, prompt string) ([]byte, error) {
		calls++
		if !strings.HasPrefix(prompt, "\x00password:") {
			t.Fatalf("unexpected prompt %q", prompt)
		}
		return []byte("correct"), nil
	}
	c, err := Dial(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if calls != 1 {
		t.Fatalf("prompts %d", calls)
	}
	policyConfig := strings.Replace(strings.Join(lines, "\n"), "\nSSHORE_CONFIG\n", "\npreferredauthentications publickey\nSSHORE_CONFIG\n", 1)
	if err := os.WriteFile(f.options.Executable, []byte(policyConfig), 0700); err != nil {
		t.Fatal(err)
	}
	if c, err := Dial(context.Background(), f.options); err == nil {
		c.Close()
		t.Fatal("password used despite PreferredAuthentications")
	}
	if calls != 1 {
		t.Fatal("excluded password method prompted")
	}
	f.options.Secret = nil
	if c, err := Dial(context.Background(), f.options); err == nil {
		c.Close()
		t.Fatal("authenticated without password")
	}
}

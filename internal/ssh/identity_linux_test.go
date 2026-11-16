package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

func TestEncryptedFileSSHAuthentication(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	fixture := newConnectionFixture(t, private)
	path := filepath.Join(filepath.Dir(fixture.options.Executable), "identity")
	block, err := cryptossh.MarshalPrivateKeyWithPassphrase(private, "", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	os.Remove(path + ".pub") // OpenSSH's embedded public key is sufficient.
	config, err := os.ReadFile(fixture.options.Executable)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(config), "\n")
	for i, line := range lines {
		if line == "SSHORE_CONFIG" {
			lines[i] = "connecttimeout 1\nSSHORE_CONFIG"
		}
	}
	for i, line := range lines {
		if strings.HasPrefix(line, "identityagent ") {
			lines[i] = "identityagent none"
		}
	}
	os.WriteFile(fixture.options.Executable, []byte(strings.Join(lines, "\n")), 0700)
	prompts := 0
	fixture.options.Passphrase = func(context.Context, string) ([]byte, error) {
		prompts++
		time.Sleep(1100 * time.Millisecond)
		return []byte("secret"), nil
	}
	client, err := Dial(context.Background(), fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if prompts != 1 {
		t.Fatalf("prompts=%d", prompts)
	}
	_, rejected, _ := ed25519.GenerateKey(rand.Reader)
	rejectedBlock, _ := cryptossh.MarshalPrivateKeyWithPassphrase(rejected, "", []byte("secret"))
	os.WriteFile(path, pem.EncodeToMemory(rejectedBlock), 0600)
	if client, err = Dial(context.Background(), fixture.options); err == nil {
		client.Close()
		t.Fatal("server accepted unrelated identity")
	}
	if prompts != 1 {
		t.Fatal("prompted for a key the server rejected")
	}
}

func TestEncryptedIdentityAllowsAgentInBatch(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	fixture := newConnectionFixture(t, private)
	cfg, err := resolveConfig(context.Background(), fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.values["identityfile"][0]
	block, _ := cryptossh.MarshalPrivateKeyWithPassphrase(private, "", []byte("secret"))
	os.WriteFile(path, pem.EncodeToMemory(block), 0600)
	os.Remove(path + ".pub")
	cfg.values["batchmode"] = []string{"yes"}
	_, conn, err := cfg.authentication(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	os.Chmod(path, 0644)
	files, _, _, err := cfg.fileIdentities(context.Background())
	if len(files) != 0 || err == nil {
		t.Fatal("permissive private key accepted")
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

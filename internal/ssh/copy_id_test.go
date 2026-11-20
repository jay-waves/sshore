package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
)

func TestCopyIDCandidates(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, publicOnly, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := cryptossh.NewSignerFromKey(publicOnly)
	if err != nil {
		t.Fatal(err)
	}
	public := cryptossh.MarshalAuthorizedKey(signer.PublicKey())
	write("first.pub", public)
	write("duplicate", public)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := cryptossh.MarshalPrivateKeyWithPassphrase(private, "test", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	write("second key", pem.EncodeToMemory(block))
	cfg := connectionConfig{
		options: Options{Directory: dir},
		values:  map[string][]string{"identityfile": {"none", "first", "missing", "duplicate", "second key"}},
	}
	identities, err := cfg.installIdentities()
	if err != nil || len(identities) != 2 {
		t.Fatalf("identity candidates: %v (%v)", identities, err)
	}
	if identities[0].path != filepath.Join(dir, "first.pub") || identities[1].path != filepath.Join(dir, "second key") {
		t.Fatalf("unexpected identity paths: %v", identities)
	}
}

package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	cryptossh "golang.org/x/crypto/ssh"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestFileIdentityLazyUnlock(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := cryptossh.MarshalPrivateKeyWithPassphrase(private, "test", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(block)
	calls := 0
	var supplied []byte
	cache := NewIdentityCache()
	defer cache.Close()
	c := &connectionConfig{values: map[string][]string{}, options: Options{IdentityCache: cache, Passphrase: func(context.Context, string) ([]byte, error) {
		calls++
		supplied = []byte("secret")
		return supplied, nil
	}}}
	signer, err := c.fileSigner(context.Background(), "key", data)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("prompted before server accepted key")
	}
	sig, err := signer.Sign(rand.Reader, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err = signer.PublicKey().Verify([]byte("hello"), sig); err != nil {
		t.Fatal(err)
	}
	for _, b := range supplied {
		if b != 0 {
			t.Fatal("passphrase retained")
		}
	}
	again, err := c.fileSigner(context.Background(), "key", data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = again.Sign(rand.Reader, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("prompts=%d", calls)
	}
	c.values["batchmode"] = []string{"yes"}
	c.options.IdentityCache = nil
	if _, err = c.fileSigner(context.Background(), "key", data); err == nil {
		t.Fatal("batch prompted")
	}
}

func TestFileIdentityWithoutAgent(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	block, err := cryptossh.MarshalPrivateKey(private, "test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "key")
	bad := filepath.Join(dir, "bad")
	os.WriteFile(good, pem.EncodeToMemory(block), 0600)
	os.WriteFile(bad, []byte("broken"), 0600)
	c := &connectionConfig{values: map[string][]string{"identityagent": {"none"}, "identityfile": {bad, good}, "identitiesonly": {"yes"}}}
	methods, conn, err := c.authentication(context.Background())
	if err != nil || len(methods) != 1 || conn != nil {
		t.Fatalf("methods=%d conn=%v err=%v", len(methods), conn, err)
	}
}

func TestConcurrentFileIdentityUnlock(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := cryptossh.MarshalPrivateKeyWithPassphrase(private, "", []byte("secret"))
	data := pem.EncodeToMemory(block)
	cache := NewIdentityCache()
	defer cache.Close()
	var prompts atomic.Int32
	c := &connectionConfig{values: map[string][]string{}, options: Options{IdentityCache: cache, Passphrase: func(context.Context, string) ([]byte, error) { prompts.Add(1); return []byte("secret"), nil }}}
	first, err := c.fileSigner(context.Background(), "key", data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.fileSigner(context.Background(), "key", data)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, signer := range []cryptossh.Signer{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := signer.Sign(rand.Reader, []byte("hello")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if prompts.Load() != 1 {
		t.Fatalf("prompts=%d", prompts.Load())
	}
}

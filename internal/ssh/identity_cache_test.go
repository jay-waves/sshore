package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

func TestIdentityCacheFixedLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cache := NewIdentityCache()
		defer cache.Close()
		data := []byte("encrypted identity")
		signer, err := cache.Add("first", data, key)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Minute)
		if _, ok := cache.Get("first", data); !ok {
			t.Fatal("identity expired early")
		}
		if _, err := cache.Add("first", data, key); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.Add("second", data, key); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if _, ok := cache.Get("first", data); ok {
			t.Fatal("cache access renewed the lifetime")
		}
		if _, ok := cache.Get("second", data); !ok {
			t.Fatal("identical public key in another file expired early")
		}
		if len(cache.entries) != 1 {
			t.Fatal("timer did not release expired identity")
		}
		signature, err := signer.Sign(rand.Reader, []byte("authentication in progress"))
		if err != nil {
			t.Fatal(err)
		}
		if err := signer.PublicKey().Verify([]byte("authentication in progress"), signature); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIdentityCacheChecksDeadlineWithoutTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cache := NewIdentityCache()
		defer cache.Close()
		if _, err := cache.Add("identity", []byte("encrypted"), key); err != nil {
			t.Fatal(err)
		}
		cache.entries["identity"].timer.Stop()
		time.Sleep(15 * time.Minute)
		if _, ok := cache.Get("identity", []byte("encrypted")); ok {
			t.Fatal("expired identity escaped deadline check")
		}
	})
}

func TestIdentityCacheFileReplacementAndClose(t *testing.T) {
	_, first, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewIdentityCache()
	defer cache.Close()
	data := []byte("first contents")
	if _, err := cache.Add("identity", data, first); err != nil {
		t.Fatal(err)
	}
	changed := []byte("second contents")
	if _, ok := cache.Get("identity", changed); ok {
		t.Fatal("changed file reused old identity")
	}
	if _, ok := cache.Get("identity", data); ok {
		t.Fatal("old file entry survived invalidation")
	}
	signer, err := cache.Add("identity", changed, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Get("identity", changed); ok {
		t.Fatal("closed cache retained identity")
	}
	if _, err := cache.Add("identity", changed, second); err == nil {
		t.Fatal("closed cache accepted identity")
	}
	signature, err := signer.Sign(rand.Reader, changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.PublicKey().Verify(changed, signature); err != nil {
		t.Fatal(err)
	}
	if _, err := NewIdentityCache().Add("invalid", nil, struct{}{}); err == nil {
		t.Fatal("invalid private key accepted")
	}
}

func TestIdentityCacheRSAAndCertificateSignatures(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewIdentityCache()
	defer cache.Close()
	signer, err := cache.Add("rsa", []byte("encrypted rsa"), private)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &cryptossh.Certificate{Key: signer.PublicKey(), CertType: cryptossh.UserCert, ValidBefore: cryptossh.CertTimeInfinity}
	if err := certificate.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	certified, err := cryptossh.NewCertSigner(certificate, signer)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []cryptossh.Signer{signer, certified} {
		algorithmSigner, ok := candidate.(cryptossh.AlgorithmSigner)
		if !ok {
			t.Fatal("RSA algorithm signing unavailable")
		}
		for _, algorithm := range []string{cryptossh.KeyAlgoRSASHA256, cryptossh.KeyAlgoRSASHA512} {
			signature, err := algorithmSigner.SignWithAlgorithm(rand.Reader, []byte("payload"), algorithm)
			if err != nil {
				t.Fatal(err)
			}
			if signature.Format != algorithm {
				t.Fatalf("wrong signing algorithm: %s", signature.Format)
			}
			if err := signer.PublicKey().Verify([]byte("payload"), signature); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestIdentityCacheConcurrentAccess(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewIdentityCache()
	defer cache.Close()
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			for range 10 {
				if _, err := cache.Add("identity", []byte("file"), private); err != nil {
					t.Error(err)
					return
				}
				if _, ok := cache.Get("identity", []byte("file")); !ok {
					t.Error("live identity disappeared")
					return
				}
			}
		})
	}
	workers.Wait()
}

func TestIdentityCacheUnlockCoordination(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cache := NewIdentityCache()
		defer cache.Close()
		release, err := cache.Acquire(context.Background(), "identity")
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan bool, 1)
		go func() {
			done, err := cache.Acquire(context.Background(), "identity")
			if err != nil {
				t.Error(err)
				result <- false
				return
			}
			defer done()
			_, ok := cache.Get("identity", []byte("file"))
			result <- ok
		}()
		synctest.Wait()
		select {
		case <-result:
			t.Fatal("same identity unlocked concurrently")
		default:
		}
		other, err := cache.Acquire(context.Background(), "other")
		if err != nil {
			t.Fatal(err)
		}
		other()
		if _, err := cache.Add("identity", []byte("file"), private); err != nil {
			t.Fatal(err)
		}
		release()
		if !<-result {
			t.Fatal("waiting unlock did not observe freshly cached signer")
		}
		synctest.Wait()
		if len(cache.unlocks) != 0 {
			t.Fatal("released unlock retained bookkeeping")
		}
	})
}

func TestIdentityCacheUnlockCancellationAndClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := NewIdentityCache()
		release, err := cache.Acquire(context.Background(), "identity")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		ctx, cancel := context.WithCancel(context.Background())
		canceled := make(chan error, 1)
		closed := make(chan error, 1)
		go func() {
			done, err := cache.Acquire(ctx, "identity")
			if done != nil {
				done()
			}
			canceled <- err
		}()
		go func() {
			done, err := cache.Acquire(context.Background(), "identity")
			if done != nil {
				done()
			}
			closed <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-canceled; err != context.Canceled {
			t.Fatalf("waiter did not cancel: %v", err)
		}
		if err := cache.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-closed; err == nil {
			t.Fatal("cache close did not abort waiting unlock")
		}
		release()
		release()
		if len(cache.unlocks) != 0 {
			t.Fatal("canceled unlock retained bookkeeping")
		}
	})
}

func TestIdentityCacheUnlockDoesNotDelayExpiration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cache := NewIdentityCache()
		defer cache.Close()
		release, err := cache.Acquire(context.Background(), "identity")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if _, err := cache.Add("identity", []byte("file"), private); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Minute)
		synctest.Wait()
		if len(cache.entries) != 0 {
			t.Fatal("active unlock blocked expiration")
		}
	})
}

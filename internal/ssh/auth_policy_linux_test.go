package ssh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

func TestFileAuthenticationPolicies(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := cryptossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &cryptossh.Certificate{Key: signer.PublicKey(), CertType: cryptossh.UserCert, ValidPrincipals: []string{"fixture"}, ValidBefore: cryptossh.CertTimeInfinity}
	if err := certificate.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	for _, identity := range []any{private, fixtureIdentity{PrivateKey: private, Certificate: certificate}} {
		fixture := newConnectionFixture(t, identity)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cfg, err := resolveConfig(ctx, fixture.options)
		if err != nil {
			t.Fatal(err)
		}
		cfg.values["requiredrsasize"] = []string{"2048"}
		if _, err := cfg.authentication(); err == nil {
			t.Fatal("undersized RSA key accepted")
		}
		cfg.values["requiredrsasize"] = []string{"1024"}
		cfg.values["preferredauthentications"] = []string{"password,keyboard-interactive"}
		if _, err := cfg.authentication(); err == nil || !strings.Contains(err.Error(), "excludes publickey") {
			t.Fatalf("public-key exclusion ignored: %v", err)
		}
		cfg.values["preferredauthentications"] = []string{"publickey"}
		if _, err := cfg.authentication(); err != nil {
			t.Fatal(err)
		}
	}
}

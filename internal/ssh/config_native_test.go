package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
)

func TestNativeConfigurationOptions(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "known hosts"), filepath.Join(dir, "backup hosts")
	cfg, err := resolveConfig(t.Context(), Options{
		Host: "operand@Fixture", Environment: []string{},
		Args: []string{"-F", "none", "-o", "Port=2211", "-p", "2222", "-l", "selected",
			"-4", "-6", "-c", "^aes128-ctr", "-i", "chosen key",
			"-o", `UserKnownHostsFile="` + first + `" "` + second + `"`,
			"-o", "GlobalKnownHostsFile=none", "-o", "ServerAliveInterval=0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.user != "selected" || cfg.host != "fixture" || cfg.port != "2222" || cfg.network != "tcp6" {
		t.Fatalf("unexpected effective connection: %+v", cfg)
	}
	if got := cfg.get("ciphers"); !strings.HasPrefix(got, "aes128-ctr,") {
		t.Fatalf("cipher preference: %q", got)
	}
	if cfg.get("serveraliveinterval") != "0" || cfg.get("serveralivecountmax") != "3" {
		t.Fatal("explicit SSH keepalive setting was replaced by defaults")
	}
	if !slices.Equal(cfg.values["identityfile"], []string{"chosen key"}) {
		t.Fatalf("identity paths: %q", cfg.values["identityfile"])
	}
	paths, writable, err := cfg.knownHostsPaths()
	if err != nil || writable != first || !slices.Equal(paths, []string{first, second}) {
		t.Fatalf("trusted paths: %q, writable %q, %v", paths, writable, err)
	}
	_, err = resolveConfig(t.Context(), Options{
		Host: "fixture", Args: []string{"-F", "none", "-l", "alice", "-o", "KexAlgorithms=-*"},
	})
	if err == nil || !strings.Contains(err.Error(), "kexalgorithms: no supported SSH algorithm remains") {
		t.Fatalf("empty transport policy: %v", err)
	}
}

func TestNativeHostCertificateSignaturePolicy(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := cryptossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	cert := &cryptossh.Certificate{
		Key: ca.PublicKey(), CertType: cryptossh.HostCert,
		ValidPrincipals: []string{"fixture"}, ValidBefore: cryptossh.CertTimeInfinity,
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	data := "@cert-authority fixture " + string(cryptossh.MarshalAuthorizedKey(ca.PublicKey()))
	data += "fixture " + string(cryptossh.MarshalAuthorizedKey(ca.PublicKey()))
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	for _, algorithm := range []string{"ssh-ed25519", "rsa-sha2-256", "ssh-rsa"} {
		cfg, err := resolveConfig(t.Context(), Options{
			Host: "fixture", Environment: []string{},
			Args: []string{"-F", "none", "-o", "StrictHostKeyChecking=yes",
				"-o", `UserKnownHostsFile="` + path + `"`, "-o", "GlobalKnownHostsFile=none",
				"-o", "CASignatureAlgorithms=" + algorithm},
		})
		if err != nil {
			t.Fatal(err)
		}
		verify, offered, err := cfg.hostKeys(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}
		if err := verify("fixture:22", remote, ca.PublicKey()); err != nil {
			t.Fatalf("CA policy affected a pinned raw host key: %v", err)
		}
		err = verify("fixture:22", remote, cert)
		if algorithm == "ssh-ed25519" && err != nil {
			t.Fatalf("trusted certificate rejected: %v", err)
		}
		if algorithm != "ssh-ed25519" && (err == nil || !strings.Contains(err.Error(), "CASignatureAlgorithms")) {
			t.Fatalf("disabled certificate signature accepted: %v", err)
		}
		if algorithm == "ssh-rsa" && slices.Contains(offered, cryptossh.CertAlgoED25519v01) {
			t.Fatalf("unusable host certificate algorithm offered: %v", offered)
		}
	}
}

package sshtest

import (
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
)

func writeConfig(t testing.TB, dir, address string, private ed25519.PrivateKey, key cryptossh.PublicKey) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte("[127.0.0.1]:"+port+" "+string(cryptossh.MarshalAuthorizedKey(key))), 0600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "identity")
	block, err := cryptossh.MarshalPrivateKey(private, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("hostname 127.0.0.1\nuser fixture\nport %s\nidentityfile %q\nidentitiesonly yes\nstricthostkeychecking yes\nuserknownhostsfile %q\n", port, filepath.ToSlash(keyPath), filepath.ToSlash(known))
	path := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

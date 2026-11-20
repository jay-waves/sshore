package sshtest

import (
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
)

func writeConfig(t testing.TB, dir, address string, private ed25519.PrivateKey, key cryptossh.PublicKey, prelude string) string {
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
	output := fmt.Sprintf("hostname 127.0.0.1\nuser fixture\nport %s\nidentityfile %s\nidentitiesonly yes\nstricthostkeychecking yes\nuserknownhostsfile %s\n", port, keyPath, known)
	shim := filepath.Join(dir, "ssh")
	body := "#!/bin/sh\n[ \"$1\" = -G ] || exit 90\n" + prelude + "\nprintf '%s' " + quote(output) + "\n"
	if err := os.WriteFile(shim, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return shim
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

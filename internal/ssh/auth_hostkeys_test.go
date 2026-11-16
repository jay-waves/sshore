package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
)

func TestKnownHostsAppendUnterminatedLine(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := cryptossh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	original := "previous " + strings.TrimSpace(string(cryptossh.MarshalAuthorizedKey(key))) + " previous-comment"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	c := &connectionConfig{host: "new-host", port: "22", values: map[string][]string{"userknownhostsfile": {path}, "stricthostkeychecking": {"accept-new"}}}
	verify, _, err := c.hostKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	if err := verify("new-host:22", remote, key); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(contents), original+"\nnew-host ") {
		t.Fatalf("existing record overwritten or new record not separated: %q", contents)
	}
	c.values["stricthostkeychecking"] = []string{"yes"}
	for _, host := range []string{"previous", "new-host"} {
		c.host = host
		verify, _, err := c.hostKeys(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := verify(host+":22", remote, key); err != nil {
			t.Fatalf("saved trust for %s lost: %v", host, err)
		}
	}
}

func TestHostKeyAliasKeepsConfiguredLookupName(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := cryptossh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"stable-alias", "[stable-alias]:2200"} {
		t.Run(alias, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "known_hosts")
			if err := os.WriteFile(path, append([]byte(alias+" "), cryptossh.MarshalAuthorizedKey(key)...), 0600); err != nil {
				t.Fatal(err)
			}
			c := &connectionConfig{host: "actual-host", port: "2222", values: map[string][]string{"hostkeyalias": {alias}, "userknownhostsfile": {path}, "stricthostkeychecking": {"yes"}}}
			verify, _, err := c.hostKeys(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := verify("actual-host:2222", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222}, key); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRequiredRSASizeHostKeys(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	key, err := cryptossh.NewPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, append([]byte("fixture "), cryptossh.MarshalAuthorizedKey(key)...), 0600); err != nil {
		t.Fatal(err)
	}
	c := &connectionConfig{host: "fixture", port: "22", values: map[string][]string{"userknownhostsfile": {path}, "stricthostkeychecking": {"yes"}, "requiredrsasize": {"2048"}}}
	verify, _, err := c.hostKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []cryptossh.PublicKey{key, &cryptossh.Certificate{Key: key, CertType: cryptossh.HostCert}} {
		err := verify("fixture:22", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}, candidate)
		if err == nil || !strings.Contains(err.Error(), "RequiredRSASize requires 2048") {
			t.Fatalf("undersized RSA host key accepted: %v", err)
		}
	}
	delete(c.values, "requiredrsasize")
	verify, _, err = c.hostKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := verify("fixture:22", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}, key); err != nil {
		t.Fatal(err)
	}
}

func TestBatchModeNeverPromptsForHostTrust(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := cryptossh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	prompted := false
	c := &connectionConfig{options: Options{Confirm: func(context.Context, string) (bool, error) { prompted = true; return true, nil }}, host: "unknown", port: "22", values: map[string][]string{"userknownhostsfile": {filepath.Join(t.TempDir(), "known_hosts")}, "stricthostkeychecking": {"ask"}, "batchmode": {"yes"}}}
	verify, _, err := c.hostKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := verify("unknown:22", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}, key); err == nil || !strings.Contains(err.Error(), "BatchMode") {
		t.Fatalf("unknown key not rejected: %v", err)
	}
	if prompted {
		t.Fatal("BatchMode asked for interactive confirmation")
	}
}

func TestLegacyPublicKeyAlgorithmPolicy(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := cryptossh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	c := &connectionConfig{values: map[string][]string{"pubkeyacceptedkeytypes": {cryptossh.KeyAlgoRSASHA512}}}
	if c.acceptsSignature(key, cryptossh.KeyAlgoED25519) {
		t.Fatal("legacy algorithm restriction ignored")
	}
	c.values["pubkeyacceptedkeytypes"] = []string{cryptossh.CertAlgoED25519v01}
	if !c.acceptsSignature(&cryptossh.Certificate{Key: key}, cryptossh.KeyAlgoED25519) || c.acceptsSignature(key, cryptossh.KeyAlgoED25519) {
		t.Fatal("legacy certificate policy lost")
	}
	c.values["pubkeyacceptedalgorithms"] = []string{cryptossh.KeyAlgoED25519}
	if !c.acceptsSignature(key, cryptossh.KeyAlgoED25519) {
		t.Fatal("modern algorithm policy did not take precedence")
	}
}

func TestKnownHostsPathLists(t *testing.T) {
	dir := t.TempDir()
	key, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := cryptossh.NewPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known hosts")
	if err := os.WriteFile(known, append([]byte("fixture "), cryptossh.MarshalAuthorizedKey(hostKey)...), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, global string
		reject       bool
	}{
		{"missing absolute files", filepath.Join(dir, "missing") + " " + filepath.Join(dir, "missing2"), false},
		{"environment paths", "${HOSTS_DIR}/missing ${HOSTS_DIR}/missing2", false},
		{"ambiguous relative paths", "missing hosts", true},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, struct {
			name, global string
			reject       bool
		}{"Windows OpenSSH defaults", `__PROGRAMDATA__\ssh/ssh_known_hosts __PROGRAMDATA__\ssh/ssh_known_hosts2`, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &connectionConfig{options: Options{Directory: dir, Environment: []string{"HOSTS_DIR=" + dir, "ProgramData=" + dir}}, host: "fixture", port: "22", values: map[string][]string{
				"userknownhostsfile": {known}, "globalknownhostsfile": {tc.global}, "stricthostkeychecking": {"yes"},
			}}
			verify, _, err := c.hostKeys(context.Background())
			if tc.reject {
				if err == nil || !strings.Contains(err.Error(), "ambiguous known_hosts") {
					t.Fatalf("expected ambiguous path rejection, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
			if err := verify("fixture:22", remote, hostKey); err != nil {
				t.Fatal(err)
			}
			other, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := cryptossh.NewPublicKey(other)
			if err != nil {
				t.Fatal(err)
			}
			if err := verify("fixture:22", remote, changed); err == nil {
				t.Fatal("accepted changed host key")
			}
		})
	}
}

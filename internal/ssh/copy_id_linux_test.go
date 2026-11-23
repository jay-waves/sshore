package ssh

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jay-waves/sshore/tests/testutil/sshtest"
	cryptossh "golang.org/x/crypto/ssh"
)

func TestCopyIDWithoutSSHKeygen(t *testing.T) {
	home, tools := t.TempDir(), t.TempDir()
	for _, name := range []string{"sh", "flock", "base64", "mkdir", "chmod", "tail"} {
		executable, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(executable, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tools)
	resolver := sshtest.Start(t, sshtest.Options{Exec: func(ctx context.Context, command string, stream cryptossh.Channel) {
		sshtest.Shell(ctx, command, stream, home, []string{"HOME=" + home, "PATH=" + tools})
	}})
	identity, err := privateIdentity(filepath.Join(filepath.Dir(resolver), "identity"))
	if err != nil {
		t.Fatal(err)
	}
	key := cryptossh.MarshalAuthorizedKey(identity.PublicKey())
	certificate := &cryptossh.Certificate{Key: identity.PublicKey(), CertType: cryptossh.UserCert}
	if err := certificate.SignCert(rand.Reader, identity); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "authorized_keys")
	restricted := `command="printf \"hello world\"",no-pty ` + string(key)
	certified := string(cryptossh.MarshalAuthorizedKey(certificate))
	comment := "# " + strings.TrimSuffix(string(key), "\n")

	for _, tc := range []struct {
		name, existing, want string
		clients              int
	}{
		{"empty file", "", string(key), 1},
		{"restricted key", restricted, restricted, 1},
		{"certificate with the same key", certified, certified, 1},
		{"concurrent append after unterminated comment", comment, comment + "\n" + string(key), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(file, []byte(tc.existing), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var clients sync.WaitGroup
			for range tc.clients {
				clients.Go(func() {
					o := Options{Host: "fixture", Args: []string{"-F", resolver}, Environment: []string{"HOME=" + home, "PATH=" + tools}}
					if err := CopyID(ctx, o, nil, io.Discard); err != nil {
						t.Error(err)
					}
				})
			}
			clients.Wait()
			got, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("authorized_keys = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("incomplete snapshot", func(t *testing.T) {
		snapshot := copyIDHeader + "\n" + base64.StdEncoding.EncodeToString(key) + "\n"
		reader := bufio.NewReader(strings.NewReader(snapshot))
		if _, err := copyIDKeyLine(reader, identity.PublicKey()); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("expected a truncated snapshot error, got %v", err)
		}
	})
}

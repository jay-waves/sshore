package ssh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigResolverOpenSSHCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		rejected    bool
	}{
		{"older Windows OpenSSH fields", "", false},
		{"disabled revocation file", "revokedhostkeys none\n", false},
		{"revocation file must not be ignored", "revokedhostkeys /path/to/revoked_keys\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "ssh-config")
			script := `#!/bin/sh
for arg; do
 case "$arg" in ForkAfterAuthentication=*|SessionType=*) echo 'unsupported option' >&2; exit 1;; esac
done
cat <<'CONFIG'
hostname fixture
user fixture
port 22
stricthostkeychecking yes
` + tc.extra + "CONFIG\n"
			if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			_, err := resolveConfig(context.Background(), Options{Host: "fixture", Executable: exe})
			if tc.rejected {
				if err == nil || !strings.Contains(err.Error(), "revokedhostkeys is not supported") {
					t.Fatalf("expected unsupported revocation config, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConfigCancellationWithMatchExec(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("system OpenSSH is unavailable")
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("Match exec \"sleep 2\"\n  User fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = resolveConfig(ctx, Options{Host: "fixture", Executable: ssh, Args: []string{"-F", path}})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expected canceled configuration, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Match exec retained configuration pipes for %v", elapsed)
	}
}

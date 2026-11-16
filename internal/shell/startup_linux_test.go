package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBashStartupFiles(t *testing.T) {
	path, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	home := t.TempDir()
	for file, value := range map[string]string{".bash_profile": "LOGIN", ".bashrc": "INTERACTIVE"} {
		if err := os.WriteFile(filepath.Join(home, file), []byte("export SSHORE_STARTUP_TEST="+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, noLogin := range []bool{false, true} {
		args, err := StartupArgs(path, noLogin, false)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, path, append(args, "-c", `printf '\nSTARTUP:%s\n' "$SSHORE_STARTUP_TEST"`)...)
		cmd.Env = append(os.Environ(), "HOME="+home, "SSHORE_STARTUP_TEST=", "TERM=dumb")
		out, err := cmd.CombinedOutput()
		cancel()
		want := "STARTUP:LOGIN"
		if noLogin {
			want = "STARTUP:INTERACTIVE"
		}
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("noLogin=%v: %v %s", noLogin, err, out)
		}
	}
}

func TestNoProfileStartupFiles(t *testing.T) {
	for _, name := range []string{"bash", "sh"} {
		t.Run(name, func(t *testing.T) {
			path, err := exec.LookPath(name)
			if err != nil {
				t.Skip(err)
			}
			home := t.TempDir()
			for _, file := range []string{".bash_profile", ".bashrc", ".profile", "env-hook"} {
				if err := os.WriteFile(filepath.Join(home, file), []byte("export SSHORE_PROFILE_RAN=yes\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args, err := StartupArgs(path, false, true)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, path, append(args, "-c", `printf '\nRESULT:%s:%s\n' "$SSHORE_PROFILE_RAN" "$SSHORE_INHERITED"`)...)
			source := append(os.Environ(), "HOME="+home, "TERM=dumb", "SSHORE_PROFILE_RAN=", "SSHORE_INHERITED=keep", "ENV="+filepath.Join(home, "env-hook"))
			cmd.Env = StartupEnv(path, source, true)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "RESULT::keep") {
				t.Fatalf("%v %s", err, out)
			}
			if source[len(source)-1] != "ENV="+filepath.Join(home, "env-hook") {
				t.Fatal("parent environment mutated")
			}
		})
	}
}

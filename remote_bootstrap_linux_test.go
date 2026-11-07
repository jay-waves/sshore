package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runBootstrap(t *testing.T, home string, o options) (string, error) {
	t.Helper()
	tools := filepath.Join(home, "test-tools")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	curl := filepath.Join(tools, "curl")
	if _, err := os.Stat(curl); os.IsNotExist(err) {
		if err := os.WriteFile(curl, []byte("#!/bin/sh\nexit 79\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", remoteCommand(o))
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+tools+":/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeBootstrapStub(t *testing.T, path, release string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo " + shellQuote("sshore "+release) + "; exit 0; fi\nprintf '<%s>\\n' \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteFallbackAndArgumentQuoting(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin", "sshore")
	writeBootstrapStub(t, bin, version)
	for _, mode := range []string{"bridge", "resume", "sessions", "new", "attach"} {
		o := options{server: "sshore", session: "main", list: mode == "sessions", command: mode}
		if mode == "resume" {
			o.approved = []operation{{Kind: "forward", Port: 80}}
		}
		out, err := runBootstrap(t, home, o)
		want := "<bootstrap>\n<" + bin + ">\n<" + mode + ">\n<main>\n"
		if err != nil || out != want {
			t.Fatal(out, err)
		}
	}
	odd := filepath.Join(home, "server ' $(touch INJECTED)")
	if err := os.Rename(bin, odd); err != nil {
		t.Fatal(err)
	}
	out, err := runBootstrap(t, home, options{server: odd, session: "main"})
	if err != nil || !strings.Contains(out, "<"+odd+">") {
		t.Fatal(out, err)
	}
	if _, err := os.Stat("INJECTED"); !os.IsNotExist(err) {
		t.Fatal("shell interpolation occurred", err)
	}
}

func TestRemoteNewerRejectedWithoutDownload(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "server")
	writeBootstrapStub(t, bin, "999.0.0")
	out, err := runBootstrap(t, home, options{server: bin, session: "main"})
	if err == nil || !strings.Contains(out, "newer") || strings.Contains(out, "Downloading") {
		t.Fatal(out, err)
	}
}

func TestRemoteStagesWithoutReplacing(t *testing.T) {
	for _, release := range []string{"", "0.0.1", "00.4.5", "custom", "broken"} {
		home := t.TempDir()
		bin := filepath.Join(home, "server")
		if release != "" {
			writeBootstrapStub(t, bin, release)
			if release == "broken" {
				if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 23\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
		}
		original, _ := os.ReadFile(bin)
		prepared := filepath.Join(home, "prepared")
		writeBootstrapStub(t, prepared, version)
		t.Setenv("SSHORE_SETUP_TEST_BINARY", prepared)
		tools := filepath.Join(home, "test-tools")
		if err := os.MkdirAll(tools, 0700); err != nil {
			t.Fatal(err)
		}
		curl := `#!/bin/sh
while [ "$#" -gt 0 ]; do
 if [ "$1" = -o ]; then shift; output=$1; fi
 shift
done
cat > "$output" <<'INSTALL'
#!/bin/sh
if [ "$SSHORE_SETUP_TEST_FAIL" = 1 ]; then
 echo 'checksum verification failed' >&2
 exit 1
fi
mkdir -p "$4"
cp "$SSHORE_SETUP_TEST_BINARY" "$4/sshore"
echo 'Directory is not on PATH. staging-only-output'
echo 'staging-only-stderr' >&2
INSTALL
`
		if err := os.WriteFile(filepath.Join(tools, "curl"), []byte(curl), 0700); err != nil {
			t.Fatal(err)
		}
		out, err := runBootstrap(t, home, options{server: bin, session: "main"})
		if err != nil || !strings.Contains(out, "<bootstrap>\n<"+bin+">") {
			t.Fatal(out, err)
		}
		if strings.Contains(out, "staging-only") || strings.Contains(out, "PATH") {
			t.Fatal("temporary installer output leaked", out)
		}
		if release != "" {
			unchanged, err := os.ReadFile(bin)
			if err != nil || string(unchanged) != string(original) {
				t.Fatal("bootstrap overwrote target", err)
			}
		} else if _, err := os.Stat(bin); !os.IsNotExist(err) {
			t.Fatal("bootstrap overwrote target", err)
		}
		t.Setenv("SSHORE_SETUP_TEST_FAIL", "1")
		out, err = runBootstrap(t, home, options{server: bin, session: "main"})
		if err == nil || !strings.Contains(out, "checksum verification failed") || strings.Contains(out, "<bootstrap>") {
			t.Fatal("installer failure must retain diagnostics and stop setup", out, err)
		}
		t.Setenv("SSHORE_SETUP_TEST_FAIL", "")
	}
}

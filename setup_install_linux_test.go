//go:build sshoretest

package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestInstallerPreparedUpdate(t *testing.T) {
	e := newEnv(t)
	shell, pid := e.attach("existing")
	defer shell.close()
	target := filepath.Join(e.dir, "installed", "sshore")
	writeBootstrapStub(t, target, "0.0.1")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	run := func(answer string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, e.bin, "bootstrap", target)
		cmd.Env = append(e.env, "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=")
		terminal, err := pty.Start(cmd)
		if err != nil {
			t.Fatal(err)
		}
		defer terminal.Close()
		defer func() { cmd.Process.Kill(); cmd.Wait() }()
		var output strings.Builder
		chunk := make([]byte, 1024)
		for !strings.Contains(output.String(), "Type update to install") {
			n, err := terminal.Read(chunk)
			output.Write(chunk[:n])
			if err != nil {
				t.Fatalf("confirmation missing: %s: %v", &output, err)
			}
		}
		if !strings.Contains(output.String(), "existing") {
			t.Fatalf("task not listed: %s", &output)
		}
		if _, err := io.WriteString(terminal, answer+"\n"); err != nil {
			t.Fatal(err)
		}
		for {
			n, err := terminal.Read(chunk)
			output.Write(chunk[:n])
			if err != nil {
				break
			}
		}
		return output.String(), cmd.Wait()
	}
	if out, err := run(""); err == nil || !strings.Contains(out, "no tasks ended") {
		t.Fatalf("declined install: %v %s", err, out)
	}
	if current, err := os.ReadFile(target); err != nil || string(current) != string(original) {
		t.Fatal("decline replaced executable", err)
	}
	if current, err := readSetupTask(pid); err != nil || current.state == "Z" {
		t.Fatalf("decline ended shell: %+v %v", current, err)
	}
	if out, err := run("update"); err != nil {
		t.Fatalf("update: %v %s", err, out)
	}
	if got, err := executableVersion(target); err != nil || got != version {
		t.Fatalf("installed: %s %v", got, err)
	}
	waitSessionEnded(t, e, "existing", pid)
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".sshore-update-*")); len(matches) != 0 {
		t.Fatal("staging files remain", matches)
	}
}

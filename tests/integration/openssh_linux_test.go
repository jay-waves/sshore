//go:build linux

package tests_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

func TestOpenSSH(t *testing.T) {
	binary := os.Getenv("SSHORE_TEST_OPENSSH")
	if binary == "" {
		t.Skip("run in the disposable OpenSSH Docker fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:2222", time.Second)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("sshd did not start:", err)
		case <-time.After(50 * time.Millisecond):
		}
	}

	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexit 97\n"), 0700); err != nil {
		t.Fatal(err)
	}
	host := "fixture-alias"
	config := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(config, []byte(`Host fixture-alias
    HostName 127.0.0.1
    User sshore-fixture
    IdentityFile /tmp/fixture-key
    StrictHostKeyChecking yes
    UserKnownHostsFile /tmp/fixture-known-hosts
    Ciphers ^aes128-ctr
    PreferredAuthentications gssapi-with-mic,publickey,password
Match final all
    Port 2222
    GSSAPIAuthentication yes
Match command "printf forwarding-check"
    RemoteForward 18889 localhost:18890
`), 0600); err != nil {
		t.Fatal(err)
	}
	ssh := []string{"--", "-F", config}
	command := func(args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, binary, append(args, ssh...)...)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := command(args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	connection := strings.Fields(run("exec", host, `printf '%s\n' "$SSH_CONNECTION"`))
	if len(connection) != 4 || connection[0] != "127.0.0.1" || connection[2] != "127.0.0.1" || connection[3] != "2222" {
		t.Fatalf("SSH_CONNECTION from sshd = %q", connection)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, args := range [][]string{
		{"exec", host, "printf forwarding-check"},
		{"exec", "-L" + listener.Addr().String() + ":localhost:80", host, "printf forwarding-check"},
	} {
		cmd := command(args...)
		var warning bytes.Buffer
		cmd.Stderr = &warning
		output, err := cmd.Output()
		if err != nil || string(output) != "forwarding-check" || strings.Count(warning.String(), "WARNING: Ignoring SSH forwarding rules") != 1 {
			t.Fatalf("ignored forwarding: stdout %q, stderr %q, error %v", output, warning.String(), err)
		}
	}
	out := run("sh", host+":", "--detach", "--no-profile", "--no-login", "--server", binary)
	endpoint := ""
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "[sshore]" && fields[1] == "Started" {
			endpoint = fields[2]
			break
		}
	}
	if !strings.HasPrefix(endpoint, host+":") {
		t.Fatalf("detached launch: %q", out)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanup, binary, append([]string{"kill", endpoint, "--server", binary}, ssh...)...).Run()
	})
	attach := func(input string, markers ...string) {
		t.Helper()
		cmd := command("attach", endpoint, "--server", binary, "-D1080")
		cmd.Env = append(os.Environ(), "NO_COLOR=")
		terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 120})
		if err != nil {
			t.Fatal(err)
		}
		defer terminal.Close()
		stop := context.AfterFunc(ctx, func() { terminal.Close() })
		defer stop()
		defer func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}()
		reader := bufio.NewReader(terminal)
		wait := func(marker string) string {
			t.Helper()
			var transcript strings.Builder
			for !strings.Contains(ansi.Strip(transcript.String()), marker) {
				chunk, err := reader.ReadString('\n')
				transcript.WriteString(chunk)
				if err != nil {
					t.Fatalf("waiting for %q: %v\n%s", marker, err, transcript.String())
				}
			}
			return transcript.String()
		}
		if warning := wait("WARNING: Ignoring SSH forwarding rules"); !strings.Contains(warning, "\x1b[33m") {
			t.Fatalf("forwarding warning lacks terminal color: %q", warning)
		}
		wait("Connected ")
		if _, err := io.WriteString(terminal, input); err != nil {
			t.Fatal(err)
		}
		for _, marker := range markers {
			wait(marker)
		}
		wait("No shell attached.")
		if _, err := io.WriteString(terminal, "exit\r"); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	attach("export SSHORE_FIXTURE=retained\n\x02d", "Detached from")
	attach("printf 'VALUE=%s:%s\\n' \"$(id -un)\" \"$SSHORE_FIXTURE\"\nexit\n",
		"VALUE=sshore-fixture:retained", "Exited ")
}

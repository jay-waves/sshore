//go:build linux

package tests_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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

	host := "sshore-fixture@127.0.0.1"
	ssh := []string{"--", "-F", "/dev/null", "-p", "2222", "-i", "/tmp/fixture-key", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=/tmp/fixture-known-hosts"}
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
	out := run("launch", host+":", "--detach", "--no-profile", "--no-login", "--server", binary)
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
		cmd := command("attach", endpoint, "--server", binary)
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
		wait := func(marker string) {
			t.Helper()
			var transcript strings.Builder
			for !strings.Contains(transcript.String(), marker) {
				chunk, err := reader.ReadString('\n')
				transcript.WriteString(chunk)
				if err != nil {
					t.Fatalf("waiting for %q: %v\n%s", marker, err, transcript.String())
				}
			}
		}
		wait("Connected ")
		if _, err := io.WriteString(terminal, input); err != nil {
			t.Fatal(err)
		}
		for _, marker := range markers {
			wait(marker)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	attach("export SSHORE_FIXTURE=retained\n\x02d", "Detached from")
	attach("printf 'VALUE=%s:%s\\n' \"$(id -un)\" \"$SSHORE_FIXTURE\"\nexit\n",
		"VALUE=sshore-fixture:retained", "Exited ")
}

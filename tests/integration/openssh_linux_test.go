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
	"strconv"
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
	t.Run("reverse forward direction in list", func(t *testing.T) {
		probe, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		remotePort := strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
		probe.Close()
		localPort := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		start, err := command("forward", host+":"+remotePort, "here:"+localPort).CombinedOutput()
		if err != nil {
			t.Fatalf("start reverse forward: %s (%v)", start, err)
		}
		_, task, found := strings.Cut(string(start), "Started forward/")
		name, _, _ := strings.Cut(task, ":")
		if !found || name == "" {
			t.Fatalf("reverse forward did not start: %s", start)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exec.CommandContext(cleanup, binary, "kill", "forward/"+name).Run()
		})
		inventory, err := exec.CommandContext(ctx, binary, "list").CombinedOutput()
		if err != nil || !strings.Contains(string(inventory), "local:"+localPort+" <- "+host+":"+remotePort) ||
			strings.Contains(string(inventory), "REMOTE") {
			t.Fatalf("reverse forward inventory: %s (%v)", inventory, err)
		}
	})
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
	cleanRemote := func(endpoint string) {
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exec.CommandContext(cleanup, binary, append([]string{"kill", endpoint, "--server", binary}, ssh...)...).Run()
		})
	}
	cleanRemote(endpoint)
	attach := func(input string, markers ...string) {
		t.Helper()
		cmd := command("attach", endpoint, "--server", binary, "-D1080")
		runOpenSSHTerminal(t, ctx, cmd, func(send func(string), wait func(string) string) {
			if warning := wait("WARNING: Ignoring SSH forwarding rules"); !strings.Contains(warning, "\x1b[33m") {
				t.Fatalf("forwarding warning lacks terminal color: %q", warning)
			}
			wait("Connected ")
			send(input)
			for _, marker := range markers {
				wait(marker)
			}
			wait("No shell attached.")
			send("exit\r")
		})
	}
	attach("export SSHORE_FIXTURE=retained\n\x02d", "Detached from")
	attach("printf 'VALUE=%s:%s\\n' \"$(id -un)\" \"$SSHORE_FIXTURE\"\nexit\n",
		"VALUE=sshore-fixture:retained", "Exited ")

	t.Run("remote child resumes after its parent exits", func(t *testing.T) {
		child := host + ":build"
		cleanRemote(child)
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		parent := exec.CommandContext(ctx, binary, "sh", "here:dev", "--no-profile", "--no-login")
		const report = "printf 'BUILD_%s=%s:%s\\n' state \"$$\" \"$SSHORE_BUILD\"\n"
		var recorded string
		runOpenSSHTerminal(t, ctx, parent, func(send func(string), wait func(string) string) {
			wait("Connected here:dev")
			send(quote(binary) + " sh " + child + " --no-profile --no-login --server " + quote(binary) + " -- -F " + quote(config) + "\n")
			wait("Connected " + child)
			send("export SSHORE_BUILD=retained; " + report)
			const marker = "BUILD_state="
			line := ansi.Strip(wait(marker))
			_, state, _ := strings.Cut(strings.TrimSpace(line), marker)
			pid, value, _ := strings.Cut(state, ":")
			if number, err := strconv.Atoi(pid); err != nil || number <= 0 || value != "retained" {
				t.Fatalf("unexpected remote shell state: %q", line)
			}
			recorded = marker + state
			send("\x02d")
			wait("Connected here:dev")
			send(quote(binary) + " attach " + child + "\n")
			wait("Connected " + child)
			send(report)
			wait(recorded)
			send("\x02d")
			wait("Connected here:dev")
			send("exit\n")
			wait("Exited here:dev")
			wait("No shell attached.")
			send("exit\r")
		})
		runOpenSSHTerminal(t, ctx, command("attach", child, "--server", binary), func(send func(string), wait func(string) string) {
			wait("Connected " + child)
			send(report)
			wait(recorded)
			send("exit\n")
			wait("Exited " + child)
			wait("No shell attached.")
			send("exit\r")
		})
	})
	t.Run("nested exit explains retained roots", func(t *testing.T) {
		build, child := host+":nested-build", host+":saved-child"
		cleanRemote(child)
		cleanRemote(build)
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		parent := exec.CommandContext(ctx, binary, "sh", "here:panel-dev", "--no-profile", "--no-login")
		runOpenSSHTerminal(t, ctx, parent, func(send func(string), wait func(string) string) {
			wait("Connected here:panel-dev")
			send(quote(binary) + " sh " + build + " --no-profile --no-login --server " + quote(binary) + " -- -F " + quote(config) + "\n")
			wait("Connected " + build)
			send(quote(binary) + " sh here:saved-child --no-profile --no-login\n")
			wait("Connected " + child)
			send("export SSHORE_PANEL_CHECK=kept\n\x02d")
			wait("Connected " + build)
			send("exit\n")
			wait("Exited " + build + " (code 0)")
			wait("Pinned shells become independent roots")
			wait("(saved-child) · pinned indefinitely")
			wait("Use exit or Esc to return to here:panel-dev.")
			send("exit\r")
			wait("Connected here:panel-dev")
			send("exit\n")
			wait("No shell attached.")
			send("exit\r")
		})
		runOpenSSHTerminal(t, ctx, command("attach", child, "--server", binary), func(send func(string), wait func(string) string) {
			wait("Connected " + child)
			send("printf 'SAVED_%s=%s\n' state \"$SSHORE_PANEL_CHECK\"\nexit\n")
			wait("SAVED_state=kept")
			wait("No shell attached.")
			send("exit\r")
		})
	})
}

func runOpenSSHTerminal(t *testing.T, ctx context.Context, cmd *exec.Cmd, run func(send func(string), wait func(string) string)) {
	t.Helper()
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
	var transcript strings.Builder
	wait := func(marker string) string {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			transcript.WriteString(line)
			if strings.Contains(ansi.Strip(line), marker) {
				return line
			}
			if err != nil {
				t.Fatalf("waiting for %q: %v\n%s", marker, err, transcript.String())
			}
		}
	}
	send := func(input string) {
		t.Helper()
		if _, err := io.WriteString(terminal, input); err != nil {
			t.Fatal(err)
		}
	}
	run(send, wait)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("terminal client: %v\n%s", err, transcript.String())
	}
}

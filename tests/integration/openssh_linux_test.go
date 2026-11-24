//go:build integration && linux

package tests_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	t.Run("ssh trust and configuration", func(t *testing.T) {
		connection := strings.Fields(run("exec", host, `printf '%s\n' "$SSH_CONNECTION"`))
		if len(connection) != 4 || connection[0] != "127.0.0.1" || connection[2] != "127.0.0.1" || connection[3] != "2222" {
			t.Fatalf("SSH_CONNECTION from sshd = %q", connection)
		}
		wrongKey, err := os.ReadFile("/tmp/fixture-key.pub")
		if err != nil {
			t.Fatal(err)
		}
		knownHosts := filepath.Join(dir, "wrong-known-hosts")
		if err := os.WriteFile(knownHosts, append([]byte("[127.0.0.1]:2222 "), wrongKey...), 0600); err != nil {
			t.Fatal(err)
		}
		untrusted := exec.CommandContext(ctx, binary, "exec", host, "printf unexpected",
			"--", "-F", config, "-o", "UserKnownHostsFile="+knownHosts)
		if out, err := untrusted.CombinedOutput(); err == nil || !strings.Contains(string(out), "key mismatch") || strings.Contains(string(out), "unexpected") {
			t.Fatalf("untrusted host: %s (%v)", out, err)
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
	})
	t.Run("copy preserves destinations and rejects a different release", func(t *testing.T) {
		src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "download")
		const payload = "file from the client\n"
		if err := os.WriteFile(src, []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		remote := host + ":integration-copy"
		run("cp", src, remote)
		run("cp", remote, dst)
		requireFile(t, dst, payload)
		if err := os.WriteFile(src, []byte("replacement"), 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := command("cp", src, remote).CombinedOutput(); err == nil || !strings.Contains(string(out), "already exists") {
			t.Fatalf("copy over existing remote file: %v\n%s", err, out)
		}
		if got := run("exec", host, "cat integration-copy"); got != payload {
			t.Fatalf("upload changed the existing file: %q", got)
		}

		absent := filepath.Join(dir, "incompatible")
		other := filepath.Join(filepath.Dir(binary), "sshore-other")
		cmd := exec.CommandContext(ctx, other, "cp", remote, absent, "--", "-F", config)
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "version mismatch") {
			t.Fatalf("incompatible copy: %v\n%s", err, out)
		}
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			t.Fatalf("incompatible copy created the destination: %v", err)
		}
	})

	t.Run("file routed through a shell requires consent", func(t *testing.T) {
		run("exec", host, "printf 'approved content' > approval-source")
		declined, approved := filepath.Join(dir, "declined"), filepath.Join(dir, "approved")
		endpoint := host + ":approval"
		cmd := command("sh", endpoint, "--no-profile", "--server", binary)
		runOpenSSHTerminal(t, ctx, cmd, func(send func(string), wait terminalWait) {
			wait("Connected " + endpoint)
			send("sshore cp here:approval-source origin:" + declined + "\n")
			wait("Approve this request? [y/N]", true)
			send("\r")
			wait("approval cancelled")
			wait("Connected " + endpoint)
			if _, err := os.Stat(declined); !os.IsNotExist(err) {
				t.Fatalf("declined copy created the destination: %v", err)
			}
			send("sshore cp here:approval-source origin:" + approved + "\n")
			wait("Approve this request? [y/N]", true)
			send("y\r")
			wait("Save as:", true)
			wait(approved, true)
			send("\r")
			wait("Return to session after operation? [Y/n]", true)
			send("\r")
			wait("Saved " + approved)
			wait("Connected " + endpoint)
			requireFile(t, approved, "approved content")
			send("exit\n")
			wait("No shell attached.")
			send("exit\r")
		})
	})

	t.Run("reverse forward relays data", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
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
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				defer conn.Close()
				_, _ = io.WriteString(conn, "forwarded across OpenSSH\n")
			}
		}()
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", "127.0.0.1:"+remotePort)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if got, err := io.ReadAll(conn); err != nil || string(got) != "forwarded across OpenSSH\n" {
			t.Fatalf("reverse forward delivered %q (%v)", got, err)
		}
	})
	if os.Getenv("SSHORE_TEST_FUSE") == "1" {
		t.Run("native mount keeps open handles across replacement", func(t *testing.T) {
			run("exec", host, "mkdir mount-source && printf original > mount-source/target")
			point := t.TempDir()
			start := run("mount", host+":mount-source", "here:"+point)
			_, task, _ := strings.Cut(start, "Started mount/")
			name, _, _ := strings.Cut(task, ":")
			if name == "" {
				t.Fatalf("missing mount name: %s", start)
			}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if out, err := exec.CommandContext(cleanup, binary, "kill", "mount/"+name).CombinedOutput(); err != nil {
					t.Errorf("unmount: %s (%v)", out, err)
				}
			})
			target, replacement := filepath.Join(point, "target"), filepath.Join(point, "replacement")
			old, err := os.Open(target)
			if err != nil {
				t.Fatal(err)
			}
			defer old.Close()
			if err := os.WriteFile(replacement, []byte("updated"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, target); err != nil {
				t.Fatal(err)
			}
			if got, err := io.ReadAll(old); err != nil || string(got) != "original" {
				t.Fatalf("open handle followed the replacement: %q (%v)", got, err)
			}
			requireFile(t, target, "updated")
			if got := run("exec", host, "cat mount-source/target"); got != "updated" {
				t.Fatalf("mount did not publish the replacement: %q", got)
			}
		})
	}
	cleanRemote := func(endpoint string) {
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exec.CommandContext(cleanup, binary, append([]string{"kill", endpoint, "--server", binary}, ssh...)...).Run()
		})
	}
	t.Run("declining shutdown preserves the detached root", func(t *testing.T) {
		endpoint := host + ":resume"
		runOpenSSHTerminal(t, ctx, command("sh", endpoint, "--pin", "--no-profile", "--no-login", "--server", binary), func(send func(string), wait terminalWait) {
			wait("Connected " + endpoint)
			send("\x02d")
			wait("Detached from ")
			wait("No shell attached.")
			send("exit\r")
		})
		cleanRemote(endpoint)
		attach := func(input string, markers ...string) {
			t.Helper()
			cmd := command("attach", endpoint, "--server", binary, "-D1080")
			runOpenSSHTerminal(t, ctx, cmd, func(send func(string), wait terminalWait) {
				wait("WARNING: Ignoring SSH forwarding rules")
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
		cancelShutdown := command("kill", "--all", host, "--server", binary)
		cancelShutdown.Stdin = strings.NewReader("n\n")
		if out, err := cancelShutdown.CombinedOutput(); err == nil || !strings.Contains(string(out), "cancelled; no tasks ended") {
			t.Fatalf("declined shutdown: %s (%v)", out, err)
		}
		attach("printf 'VALUE=%s:%s\\n' \"$(id -un)\" \"$SSHORE_FIXTURE\"\nexit\n",
			"VALUE=sshore-fixture:retained", "Exited ")

	})

	t.Run("broken SSH connection follows the root pin", func(t *testing.T) {
		type shell struct {
			Name, Instance, State string
			PID                   int
		}
		lookup := func(name string) shell {
			t.Helper()
			var inventory struct{ Shells []shell }
			if err := json.Unmarshal([]byte(run("exec", host, "sshore list --json")), &inventory); err != nil {
				t.Fatal(err)
			}
			for _, task := range inventory.Shells {
				if task.Name == name {
					return task
				}
			}
			return shell{}
		}

		for _, pinned := range []bool{false, true} {
			name := "lost-temporary"
			if pinned {
				name = "lost-pinned"
			}
			endpoint := host + ":" + name
			port, drop := interruptibleSSH(t, ctx)
			args := []string{"sh", endpoint, "--server", binary, "--no-profile"}
			if pinned {
				args = append(args, "--pin")
			}
			args = append(args, "--", "-F", config, "-p", port, "-o", "HostKeyAlias=127.0.0.1:2222")
			cmd := exec.CommandContext(ctx, binary, args...)
			var original shell
			out, err := runTerminal(t, ctx, cmd, func(send func(string), wait terminalWait) {
				wait("Connected " + endpoint)
				send("export DROP_MARKER=retained; printf 'DROP_%s\\n' ready\n")
				wait("DROP_ready")
				original = lookup(name)
				if original.Instance == "" || original.PID <= 0 || original.State != "active" {
					t.Fatalf("shell was not active before cutting SSH: %+v", original)
				}
				drop()
			})
			if err == nil || !strings.Contains(ansi.Strip(out), "Disconnected from "+endpoint) {
				t.Fatalf("cutting SSH did not disconnect %s: %v\n%s", endpoint, err, out)
			}

			deadline := time.Now().Add(5 * time.Second)
			for {
				task := lookup(name)
				if !pinned && task.Instance == "" {
					probe := "if test -d /proc/" + strconv.Itoa(original.PID) + "; then echo live; fi"
					if run("exec", host, probe) == "" {
						break
					}
				}
				if pinned && task.State == "detached" {
					if task.Instance != original.Instance || task.PID != original.PID {
						t.Fatalf("pinned shell changed identity after disconnect: %+v → %+v", original, task)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("shell lifetime did not settle (pinned=%t, original PID=%d): %+v", pinned, original.PID, task)
				}
				time.Sleep(50 * time.Millisecond)
			}
			if pinned {
				runOpenSSHTerminal(t, ctx, command("attach", endpoint, "--server", binary), func(send func(string), wait terminalWait) {
					wait("Connected " + endpoint)
					send("printf 'DROP_%s=%s:%s\\n' state \"$$\" \"$DROP_MARKER\"\nexit\n")
					wait("DROP_state=" + strconv.Itoa(original.PID) + ":retained")
					wait("No shell attached.")
					send("exit\r")
				})
			}
		}
	})

	t.Run("remote child resumes after its parent exits", func(t *testing.T) {
		child := host + ":build"
		cleanRemote(child)
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		parent := exec.CommandContext(ctx, binary, "sh", "here:dev", "--no-profile", "--no-login")
		const report = "printf 'BUILD_%s=%s:%s\\n' state \"$$\" \"$SSHORE_BUILD\"\n"
		var recorded string
		runOpenSSHTerminal(t, ctx, parent, func(send func(string), wait terminalWait) {
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
			out, err := runTerminal(t, ctx, command("attach", child, "--server", binary),
				func(_ func(string), wait terminalWait) { wait("from its parent") })
			if err == nil {
				t.Fatalf("a separate frontend entered the child before its parent exited:\n%s", out)
			}
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
		runOpenSSHTerminal(t, ctx, command("attach", child, "--server", binary), func(send func(string), wait terminalWait) {
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
		runOpenSSHTerminal(t, ctx, parent, func(send func(string), wait terminalWait) {
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
		runOpenSSHTerminal(t, ctx, command("attach", child, "--server", binary), func(send func(string), wait terminalWait) {
			wait("Connected " + child)
			send("printf 'SAVED_%s=%s\n' state \"$SSHORE_PANEL_CHECK\"\nexit\n")
			wait("SAVED_state=kept")
			wait("No shell attached.")
			send("exit\r")
		})
	})

	t.Run("shutdown confirmation rejects a replacement task", func(t *testing.T) {
		localCommand := func(args ...string) *exec.Cmd { return exec.CommandContext(ctx, binary, args...) }
		local := func(args ...string) []byte {
			t.Helper()
			out, err := localCommand(args...).CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, out)
			}
			return out
		}
		const endpoint, name = "here:replaceable", "replaceable"
		start := func() {
			t.Helper()
			runOpenSSHTerminal(t, ctx, localCommand("sh", endpoint, "--pin", "--no-profile"), func(send func(string), wait terminalWait) {
				wait("Connected " + endpoint)
				send("\x02d")
				wait("No shell attached.")
				send("exit\r")
			})
		}
		identity := func() string {
			t.Helper()
			var inventory struct {
				LocalShells []struct {
					Name, Identity string
				} `json:"local_shells"`
			}
			if err := json.Unmarshal(local("list", "--json"), &inventory); err != nil {
				t.Fatal(err)
			}
			for _, task := range inventory.LocalShells {
				if task.Name == name && strings.HasPrefix(task.Identity, "task:") {
					return strings.TrimPrefix(task.Identity, "task:")
				}
			}
			t.Fatalf("missing shell %s in %+v", name, inventory.LocalShells)
			return ""
		}
		start()
		first := identity()
		var replacement string
		out, err := runTerminal(t, ctx, localCommand("kill", "--all"), func(send func(string), wait terminalWait) {
			wait(name)
			wait("[Y/n]", true)
			local("kill", "here:"+first)
			start()
			replacement = identity()
			send("y\n")
			wait("tasks changed; review again")
		})
		if err == nil || first == replacement || identity() != replacement {
			t.Fatalf("stale confirmation affected the replacement (%s → %s): %v\n%s", first, replacement, err, out)
		}
		if out, err := localCommand("kill", "here:"+first).CombinedOutput(); err == nil {
			t.Fatalf("the retired task ID still accepts a kill: %s", out)
		}
		local("kill", "here:"+replacement)
	})

}

// Relay one real OpenSSH connection. Closing both sockets simulates transport
// loss while leaving the client terminal, sshd and other SSH connections live.
func interruptibleSSH(t *testing.T, parent context.Context) (port string, disconnect context.CancelFunc) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, drop := context.WithCancel(parent)
	done := make(chan struct{})
	t.Cleanup(func() {
		drop()
		<-done
	})
	go func() {
		defer close(done)
		stop := context.AfterFunc(ctx, func() { listener.Close() })
		defer stop()
		client, err := listener.Accept()
		listener.Close()
		if err != nil {
			return
		}
		defer client.Close()
		upstream, err := (&net.Dialer{}).DialContext(ctx, "tcp4", "127.0.0.1:2222")
		if err != nil {
			return
		}
		defer upstream.Close()
		stop = context.AfterFunc(ctx, func() { client.Close(); upstream.Close() })
		defer stop()
		copied := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, client)
			_ = upstream.(*net.TCPConn).CloseWrite()
			close(copied)
		}()
		_, _ = io.Copy(client, upstream)
		client.Close()
		<-copied
	}()
	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), drop
}

func runOpenSSHTerminal(t *testing.T, ctx context.Context, cmd *exec.Cmd, run func(send func(string), wait terminalWait)) {
	t.Helper()
	if transcript, err := runTerminal(t, ctx, cmd, run); err != nil {
		t.Fatalf("terminal client: %v\n%s", err, transcript)
	}
}

func runTerminal(t *testing.T, ctx context.Context, cmd *exec.Cmd, run func(send func(string), wait terminalWait)) (string, error) {
	t.Helper()
	cmd.Env = append(cmd.Environ(), "NO_COLOR=")
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
	transcript := terminalDialogue(t, terminal, run)
	return transcript, cmd.Wait()
}

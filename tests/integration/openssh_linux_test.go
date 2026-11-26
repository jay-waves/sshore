//go:build integration && linux

package tests_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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
	"golang.org/x/sys/unix"
)

func TestOpenSSH(t *testing.T) {
	binary := os.Getenv("SSHORE_TEST_OPENSSH")
	if binary == "" {
		t.Skip("run in the disposable Alpine OpenSSH fixture")
	}
	fixture := filepath.Dir(binary)
	startup, cancelStartup := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancelStartup()
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:2222", time.Second)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-startup.Done():
			t.Fatal("sshd did not start:", err)
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancelStartup()

	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexit 97\n"), 0700); err != nil {
		t.Fatal(err)
	}
	host := "fixture-alias"
	config := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(config, []byte(fmt.Sprintf(`Host fixture-alias
    HostName 127.0.0.1
    User sshore-fixture
    IdentityFile %q
    StrictHostKeyChecking yes
    UserKnownHostsFile %q
    Ciphers ^aes128-ctr
    PreferredAuthentications gssapi-with-mic,publickey,password
Match final all
    Port 2222
    GSSAPIAuthentication yes
Match command "printf forwarding-check"
    RemoteForward 18889 localhost:18890
`, filepath.Join(fixture, "identity"), filepath.Join(fixture, "known_hosts"))), 0600); err != nil {
		t.Fatal(err)
	}
	ssh := []string{"--ssh", "-F", config}
	command := func(ctx context.Context, args ...string) *exec.Cmd {
		for i, arg := range args {
			if arg == "--" {
				withSSH := append(append([]string{}, args[:i]...), ssh...)
				return exec.CommandContext(ctx, binary, append(withSSH, args[i:]...)...)
			}
		}
		return exec.CommandContext(ctx, binary, append(args, ssh...)...)
	}
	run := func(t *testing.T, ctx context.Context, args ...string) string {
		t.Helper()
		out, err := command(ctx, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	local := func(t *testing.T, ctx context.Context, args ...string) []byte {
		t.Helper()
		out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	type resourceRef struct {
		ID, Identity string
		ShortID      string
	}
	findResource := func(t *testing.T, out []byte, id string) resourceRef {
		t.Helper()
		kind, name, _ := strings.Cut(id, "/")
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && (fields[1] == "("+name+")" || fields[0] == name && fields[1] == kind) {
				return resourceRef{ID: id, Identity: fields[0], ShortID: fields[0]}
			}
		}
		return resourceRef{}
	}
	// Each sequential scenario owns its deadline. A hung terminal cannot
	// consume the time available to its siblings.
	scenario := func(name string, exercise func(*testing.T, context.Context)) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			exercise(t, ctx)
		})
	}
	scenario("ssh trust and configuration", func(t *testing.T, ctx context.Context) {
		if got := run(t, ctx, "sh", host, "--", "printf", "'<%s>'", "a b.txt"); got != "<a><b.txt>" {
			t.Fatalf("SSH command did not join words: %q", got)
		}
		if got := run(t, ctx, "sh", host, "--", `printf '<%s>' "a b.txt"`); got != "<a b.txt>" {
			t.Fatalf("SSH command did not preserve remote quoting: %q", got)
		}
		if got := run(t, ctx, "sh", "-p", "2222", "-i", filepath.Join(fixture, "identity"), host, "--", "printf", "'%s'", "'--help -- -p 1 -J jump -i literal'"); got != "--help -- -p 1 -J jump -i literal" {
			t.Fatalf("exec changed the remote command text: %q", got)
		}
		connection := strings.Fields(run(t, ctx, "sh", host, "--", `printf '%s\n' "$SSH_CONNECTION"`))
		if len(connection) != 4 || connection[0] != "127.0.0.1" || connection[2] != "127.0.0.1" || connection[3] != "2222" {
			t.Fatalf("SSH_CONNECTION from sshd = %q", connection)
		}
		wrongKey, err := os.ReadFile(filepath.Join(fixture, "identity.pub"))
		if err != nil {
			t.Fatal(err)
		}
		knownHosts := filepath.Join(dir, "wrong-known-hosts")
		if err := os.WriteFile(knownHosts, append([]byte("[127.0.0.1]:2222 "), wrongKey...), 0600); err != nil {
			t.Fatal(err)
		}
		untrusted := exec.CommandContext(ctx, binary, "sh", host, "--ssh", "-F", config, "-o", "UserKnownHostsFile="+knownHosts,
			"--", "printf unexpected")
		if out, err := untrusted.CombinedOutput(); err == nil || !strings.Contains(string(out), "key mismatch") || strings.Contains(string(out), "unexpected") {
			t.Fatalf("untrusted host: %s (%v)", out, err)
		}
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		for _, args := range [][]string{
			{"sh", host, "--", "printf forwarding-check"},
			{"sh", host, "--ssh", "-L" + listener.Addr().String() + ":localhost:80", "-F", config, "--", "printf forwarding-check"},
		} {
			cmd := command(ctx, args...)
			if len(args) > 4 {
				cmd = exec.CommandContext(ctx, binary, args...)
			}
			var warning bytes.Buffer
			cmd.Stderr = &warning
			output, err := cmd.Output()
			if err != nil || string(output) != "forwarding-check" || strings.Count(warning.String(), "WARNING: Ignoring SSH forwarding rules") != 1 {
				t.Fatalf("ignored forwarding: stdout %q, stderr %q, error %v", output, warning.String(), err)
			}
		}
	})
	scenario("copy preserves destinations and rejects a different release", func(t *testing.T, ctx context.Context) {
		src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "download")
		const payload = "file from the client\n"
		if err := os.WriteFile(src, []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		remote := host + ":integration-copy"
		run(t, ctx, "cp", src, remote)
		run(t, ctx, "cp", remote, dst)
		requireFile(t, dst, payload)
		if err := os.WriteFile(src, []byte("replacement"), 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := command(ctx, "cp", src, remote).CombinedOutput(); err == nil || !strings.Contains(string(out), "already exists") {
			t.Fatalf("copy over existing remote file: %v\n%s", err, out)
		}
		if got := run(t, ctx, "sh", host, "--", "cat integration-copy"); got != payload {
			t.Fatalf("upload changed the existing file: %q", got)
		}

		absent := filepath.Join(dir, "incompatible")
		other := filepath.Join(filepath.Dir(binary), "sshore-other")
		cmd := exec.CommandContext(ctx, other, "cp", remote, absent, "--ssh", "-F", config)
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "version mismatch") {
			t.Fatalf("incompatible copy: %v\n%s", err, out)
		}
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			t.Fatalf("incompatible copy created the destination: %v", err)
		}
	})

	scenario("file routed through a shell requires consent", func(t *testing.T, ctx context.Context) {
		run(t, ctx, "sh", host, "--", "printf 'approved content' > approval-source")
		declined, approved := filepath.Join(dir, "declined"), filepath.Join(dir, "approved")
		endpoint := host + ":approval"
		cmd := command(ctx, "sh", host, "--name", strings.TrimPrefix(endpoint, host+":"), "--no-profile", "--server", binary)
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
			wait("Saved " + approved)
			wait("Connected " + endpoint)
			requireFile(t, approved, "approved content")
			send("exit\n")
			wait("Exited " + endpoint)
		})
	})

	scenario("approval resumes requesting shell and resources outlive it", func(t *testing.T, ctx context.Context) {
		// The requesting shell owns the dialogue, but the approving account
		// owns the resources it creates. Ending the shell must leave them usable.
		endpoint := host + ":approval-return"
		cleanup := func() {
			_, _ = command(ctx, "kill", strings.TrimPrefix(endpoint, host+":"), "--server", binary).CombinedOutput()
		}
		cleanup()
		defer cleanup()
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		probe, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		localPort := strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
		probe.Close()
		point := t.TempDir()
		run(t, ctx, "sh", host, "--", "mkdir -p approval-lifecycle")
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		parent := exec.CommandContext(ctx, binary, "sh", "--name", "approval-parent", "--no-profile", "--no-login")
		runOpenSSHTerminal(t, ctx, parent, func(send func(string), wait terminalWait) {
			wait("Connected here:approval-parent")
			send(quote(binary) + " sh " + host + " --name approval-return --no-profile --server " + quote(binary) + " --ssh -F " + quote(config) + "\n")
			wait("Connected " + endpoint)
			send("printf '%s\\n' \"$$\" > approval-lifecycle/shell.pid && sshore mount here:approval-lifecycle origin: --read-only\n")
			wait("Approve this request? [y/N]", true)
			send("y\r")
			wait("Mountpoint (empty for automatic):", true)
			send(point + "\r")
			wait("Started mount/")
			wait("Connected " + endpoint)
			prompt := "Local target port (Enter to approve, Ctrl-C to cancel):"
			send("sshore fwd here:" + port + " origin:" + localPort + "\n")
			wait(prompt, true)
			send("\x03")
			wait("approval cancelled")
			wait("Connected " + endpoint)
			probe, err := net.Listen("tcp4", "127.0.0.1:"+localPort)
			if err != nil {
				t.Fatalf("declined forward left its listener active: %v", err)
			}
			probe.Close()
			send("sshore fwd ::" + localPort + " .:" + port + "\n")
			wait("Local source port (Enter to approve, Ctrl-C to cancel):", true)
			send("\r")
			failed := wait("forward startup failed (origin:" + localPort + " -> " + host + ":" + port + ")")
			if !strings.Contains(failed, "remote listener: listen tcp4 127.0.0.1:"+port) {
				t.Fatalf("unexpected bind error: %s", failed)
			}
			wait("Connected " + endpoint)
			send("sshore fwd here:" + port + " origin:\n")
			wait(prompt, true)
			send(localPort + "\r")
			if line := wait("Started forward/"); !strings.Contains(line, host+":"+port+" -> origin:"+localPort) {
				t.Fatalf("unexpected forward mapping: %s", line)
			}
			wait("Connected " + endpoint)
			send("exit\n")
			wait("Connected here:approval-parent")
			send("exit\n")
			wait("No shell attached.")
			send("exit\r")
		})
		// End the requesting shell as well as its frontend before exercising fresh I/O.
		// It may already have ended with the parent.
		cleanup()
		run(t, ctx, "sh", host, "--", `! kill -0 "$(cat approval-lifecycle/shell.pid)" 2>/dev/null`)
		inventory := string(local(t, ctx, "list"))
		resourceID := func(detail string) string {
			t.Helper()
			for _, line := range strings.Split(inventory, "\n") {
				if strings.Contains(line, detail) {
					return strings.Fields(line)[0]
				}
			}
			t.Fatalf("resource %q missing after shell exit:\n%s", detail, inventory)
			return ""
		}
		mountID := resourceID(point)
		defer local(t, ctx, "kill", mountID)
		forwardID := resourceID("origin:" + localPort)
		go func() {
			c, err := listener.Accept()
			if err == nil {
				defer c.Close()
				_, _ = io.WriteString(c, "still forwarding\n")
			}
		}()
		c, err := net.DialTimeout("tcp4", "127.0.0.1:"+localPort, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if got, err := io.ReadAll(c); err != nil || string(got) != "still forwarding\n" {
			t.Fatalf("new forwarded connection after shell exit: %q (%v)", got, err)
		}
		local(t, ctx, "kill", forwardID)
		run(t, ctx, "sh", host, "--", "printf independent > approval-lifecycle/after-exit")
		requireFile(t, filepath.Join(point, "after-exit"), "independent")
	})

	scenario("forward credential prompt stays in local approval screen", func(t *testing.T, ctx context.Context) {
		identity := filepath.Join(fixture, "identity")
		rawKey, err := os.ReadFile(identity)
		if err != nil {
			t.Fatal(err)
		}
		temp := t.TempDir()
		encryptedKey := filepath.Join(temp, "encrypted-key")
		if err := os.WriteFile(encryptedKey, rawKey, 0600); err != nil {
			t.Fatal(err)
		}
		keygen := exec.CommandContext(ctx, "ssh-keygen", "-p", "-P", "", "-N", "fixture-passphrase", "-f", encryptedKey)
		if out, err := keygen.CombinedOutput(); err != nil {
			t.Fatalf("encrypt fixture key: %v\n%s", err, out)
		}
		plain, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		encrypted := bytes.Replace(plain, []byte(strconv.Quote(identity)), []byte(strconv.Quote(encryptedKey)), 1)
		approvalConfig := filepath.Join(temp, "ssh_config")
		setConfig := func(data []byte) {
			t.Helper()
			if err := os.WriteFile(approvalConfig, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		setConfig(plain)
		endpoint := host + ":approval-credentials"
		cmd := exec.CommandContext(ctx, binary, "sh", host, "--name", "approval-credentials", "--no-profile", "--server", binary, "--ssh", "-F", approvalConfig)
		credential := "Passphrase for " + encryptedKey + ": "
		transcript, err := runTerminal(t, ctx, cmd, func(send func(string), wait terminalWait) {
			wait("Connected " + endpoint)
			setConfig(encrypted)
			send("sshore fwd here:9090 origin:9091\n")
			wait("Local target port (Enter to approve, Ctrl-C to cancel):", true)
			send("\r")
			wait(credential, true)
			setConfig(plain)
			send("\x03")
			wait("Connected " + endpoint)
			send("exit\n")
			wait("Exited " + endpoint)
		})
		before, _, found := strings.Cut(transcript, credential)
		if err != nil || !found || strings.LastIndex(before, "\x1b[?1049h") <= strings.LastIndex(before, "\x1b[?1049l") {
			t.Fatalf("credential prompt outside the approval screen: %v\n%s", err, transcript)
		}
	})

	scenario("reverse forward relays data", func(t *testing.T, ctx context.Context) {
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
		start, err := command(ctx, "forward", "here:"+localPort, host+":"+remotePort).CombinedOutput()
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
			_ = exec.CommandContext(cleanup, binary, "kill", name).Run()
		})
		inventory, err := exec.CommandContext(ctx, binary, "list").CombinedOutput()
		if err != nil || !strings.Contains(string(inventory), "local:"+localPort+" -> "+host+":"+remotePort) ||
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
		ref := findResource(t, local(t, ctx, "list"), "forward/"+name)
		if ref.Identity == "" || ref.ShortID == "" {
			t.Fatalf("missing identity for forward/%s", name)
		}
		local(t, ctx, "kill", ref.ShortID)
		if got := findResource(t, local(t, ctx, "list"), "forward/"+name); got.Identity != "" {
			t.Fatalf("forward still registered: %+v", got)
		}
	})
	scenario("control forward on its SSH account", func(t *testing.T, ctx context.Context) {
		probe, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
		probe.Close()
		// This dedicated fixture account may log in to its own OpenSSH server.
		run(t, ctx, "sh", host, "--", `ssh-keygen -q -t ed25519 -N '' -f task-id-key && cat task-id-key.pub >> authorized_keys`)
		out := run(t, ctx, "sh", host, "--", `sshore forward 127.0.0.1:2222 here:`+port+` --port 2222 --ssh -F none -i "$HOME/task-id-key" -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$HOME/task-id-hosts"`)
		_, description, _ := strings.Cut(out, "Started forward/")
		name, _, _ := strings.Cut(description, ":")
		if name == "" {
			t.Fatalf("forward did not start: %s", out)
		}
		ref := findResource(t, []byte(run(t, ctx, "sh", host, "--", "sshore list")), "forward/"+name)
		if ref.ShortID == "" {
			t.Fatalf("missing identity for remote forward/%s", name)
		}
		run(t, ctx, "sh", host, "--", "sshore kill "+ref.ShortID)
		if got := findResource(t, []byte(run(t, ctx, "sh", host, "--", "sshore list")), "forward/"+name); got.Identity != "" {
			t.Fatalf("remote forward still registered: %+v", got)
		}
	})
	mount := func(t *testing.T, ctx context.Context, source string, options ...string) string {
		t.Helper()
		point := t.TempDir()
		start := run(t, ctx, append([]string{"mount", host + ":" + source, "here:" + point}, options...)...)
		_, task, _ := strings.Cut(start, "Started mount/")
		name, _, _ := strings.Cut(task, ":")
		if name == "" {
			t.Fatalf("missing mount name: %s", start)
		}
		ref := findResource(t, local(t, ctx, "list"), "mount/"+name)
		if ref.ShortID == "" {
			t.Fatalf("missing identity for mount/%s", name)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(cleanup, binary, "kill", ref.ShortID).CombinedOutput(); err != nil {
				t.Errorf("unmount: %s (%v)", out, err)
				return
			}
			var stat unix.Statfs_t
			if err := unix.Statfs(point, &stat); err != nil || stat.Type == unix.FUSE_SUPER_MAGIC {
				t.Errorf("mountpoint remained active after unmount: %s (%v)", point, err)
			}
		})
		return point
	}
	scenario("mount keeps open handles across replacement", func(t *testing.T, ctx context.Context) {
		// An open handle keeps referring to the original file after the path is
		// replaced. A new open of that path must see the replacement.
		run(t, ctx, "sh", host, "--", "mkdir mount-source && printf original > mount-source/target")
		point := mount(t, ctx, "mount-source")
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
		if got := run(t, ctx, "sh", host, "--", "cat mount-source/target"); got != "updated" {
			t.Fatalf("mount did not publish the replacement: %q", got)
		}
	})
	scenario("mount file writes append and truncate", func(t *testing.T, ctx context.Context) {
		run(t, ctx, "sh", host, "--", "mkdir mount-io")
		point := mount(t, ctx, "mount-io")
		file := filepath.Join(point, "payload")
		checkData := func(want []byte) {
			t.Helper()
			got, err := os.ReadFile(file)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("mounted payload: got %d bytes with SHA-256 %x, want %d bytes with SHA-256 %x (%v)",
					len(got), sha256.Sum256(got), len(want), sha256.Sum256(want), err)
			}
		}
		data := bytes.Repeat([]byte("sshore-mount\n"), 32768)
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		checkData(data)
		f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString("appended"); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		checkData(append(data, "appended"...))
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(file, 128<<10); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("%x", sha256.Sum256(data[:128<<10]))
		if got := strings.Fields(run(t, ctx, "sh", host, "--", "sha256sum mount-io/payload")); len(got) != 2 || got[0] != want {
			t.Fatalf("remote payload after truncate: %v, want SHA-256 %s", got, want)
		}
	})
	scenario("mount creates lists renames and deletes directories", func(t *testing.T, ctx context.Context) {
		run(t, ctx, "sh", host, "--", "mkdir mount-dirs")
		point := mount(t, ctx, "mount-dirs")
		nested, renamed := filepath.Join(point, "nested"), filepath.Join(point, "renamed")
		if err := os.Mkdir(nested, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "file"), []byte("nested content"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(nested, renamed); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(point)
		if err != nil || len(entries) != 1 || entries[0].Name() != "renamed" || !entries[0].IsDir() {
			t.Fatalf("mount directory after rename: %v (%v)", entries, err)
		}
		if got := run(t, ctx, "sh", host, "--", "cat mount-dirs/renamed/file"); got != "nested content" {
			t.Fatalf("remote renamed file: %q", got)
		}
		if err := os.Remove(filepath.Join(renamed, "file")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(renamed); err != nil {
			t.Fatal(err)
		}
		run(t, ctx, "sh", host, "--", "test -z \"$(ls -A mount-dirs)\"")
	})
	scenario("mount observes remote changes and rejects escaping symlinks", func(t *testing.T, ctx context.Context) {
		run(t, ctx, "sh", host, "--", "mkdir mount-remote && printf before > mount-remote/current && "+
			"ln -s current mount-remote/inside && ln -s ../outside mount-remote/outside")
		point := mount(t, ctx, "mount-remote")
		inside := filepath.Join(point, "inside")
		requireFile(t, inside, "before")
		if _, err := os.Readlink(filepath.Join(point, "outside")); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("read escaping symlink: %v, want permission denied", err)
		}
		run(t, ctx, "sh", host, "--", "printf after > mount-remote/new && mv mount-remote/new mount-remote/current")
		deadline := time.Now().Add(3 * time.Second)
		for {
			if got, err := os.ReadFile(inside); err == nil && string(got) == "after" {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("remote replacement through mounted symlink: %q (%v)", got, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	scenario("mount read only denies changes", func(t *testing.T, ctx context.Context) {
		run(t, ctx, "sh", host, "--", "mkdir mount-ro && printf preserved > mount-ro/file")
		point := mount(t, ctx, "mount-ro", "--read-only")
		file := filepath.Join(point, "file")
		requireFile(t, file, "preserved")
		if err, want := os.WriteFile(file, []byte("changed"), 0600), unix.EROFS; !errors.Is(err, want) {
			t.Errorf("write on read-only mount: %v, want %v", err, want)
		}
		if err := os.Mkdir(filepath.Join(point, "new"), 0700); !errors.Is(err, unix.EROFS) {
			t.Errorf("mkdir on read-only mount: %v, want %v", err, unix.EROFS)
		}
		requireFile(t, file, "preserved")
	})
	cleanRemote := func(t *testing.T, endpoint string) {
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exec.CommandContext(cleanup, binary, append([]string{"kill", strings.TrimPrefix(endpoint, host+":"), "--server", binary}, ssh...)...).Run()
		})
	}
	scenario("remote program exit and cached inventory", func(t *testing.T, ctx context.Context) {
		const name = "program-inventory"
		const argument = "private fixture argument"
		cleanRemote(t, host+":"+name)
		cmd := command(ctx, "new", host, "--name", name, "--server", binary, "--", "/bin/sh", "-c",
			"printf 'ARG=%s\\n' \"$1\"; read -r reply; exit 23", "fixture", argument)
		out, err := runTerminal(t, ctx, cmd, func(send func(string), wait terminalWait) {
			wait("[Esc] Cancel", true)
			send("\r")
			wait("ARG=" + argument)
			remoteList := run(t, ctx, "sh", host, "--", binary, "list")
			if !strings.Contains(remoteList, argument) {
				t.Fatalf("managing account lost program arguments:\n%s", remoteList)
			}
			localList := string(local(t, ctx, "list"))
			if !strings.Contains(localList, name) || strings.Contains(localList, argument) {
				t.Fatalf("unexpected cached program details:\n%s", localList)
			}
			if root := os.Getenv("SSHORE_STATE_DIR"); root != "" {
				data, err := os.ReadFile(filepath.Join(root, "connections", "participation.json"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), name) || strings.Contains(string(data), argument) {
					t.Fatal("unexpected program details in participation state")
				}
			}
			send("\n")
			wait("Now in: calling terminal")
		})
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 23 {
			t.Fatalf("expected program exit status 23: %v\n%s", err, out)
		}
		if out, err := command(ctx, "kill", name).CombinedOutput(); err == nil || !strings.Contains(string(out), "unknown task "+name) {
			t.Fatalf("exited program remained selectable by name: %v\n%s", err, out)
		}
	})
	scenario("declining shutdown preserves the detached root", func(t *testing.T, ctx context.Context) {
		endpoint := host + ":resume"
		runOpenSSHTerminal(t, ctx, command(ctx, "sh", host, "--name", strings.TrimPrefix(endpoint, host+":"), "--pin", "--no-profile", "--no-login", "--server", binary), func(send func(string), wait terminalWait) {
			wait("Connected " + endpoint)
			send("\x02d")
			wait("Detached from ")
		})
		cleanRemote(t, endpoint)
		attach := func(input string, markers ...string) {
			t.Helper()
			cmd := command(ctx, "attach", strings.TrimPrefix(endpoint, host+":"), "--server", binary)
			runOpenSSHTerminal(t, ctx, cmd, func(send func(string), wait terminalWait) {
				wait("Connected ")
				send(input)
				for _, marker := range markers {
					wait(marker)
				}
			})
		}
		attach("export SSHORE_FIXTURE=retained\n\x02d", "Detached from")
		cancelShutdown := command(ctx, "kill", "--all", host, "--server", binary)
		cancelShutdown.Stdin = strings.NewReader("n\n")
		if out, err := cancelShutdown.CombinedOutput(); err == nil || !strings.Contains(string(out), "cancelled; no tasks ended") {
			t.Fatalf("declined shutdown: %s (%v)", out, err)
		}
		attach("printf 'VALUE=%s:%s\\n' \"$(id -un)\" \"$SSHORE_FIXTURE\"\nexit\n",
			"VALUE=sshore-fixture:retained", "Exited ")

	})

	scenario("remote IDs resume and missing IDs fail", func(t *testing.T, ctx context.Context) {
		root := host + ":resume-root"
		cleanRemote(t, root)
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		const marker = "RESUME_ID="
		const report = "printf 'RESUME_%s=%.6s\\n' ID \"${SSHORE_TREE_ID#task:}\"\n"
		var id string
		runOpenSSHTerminal(t, ctx, command(ctx, "shell", host, "--name", strings.TrimPrefix(root, host+":"), "--server", binary, "--no-profile", "--no-login"), func(send func(string), wait terminalWait) {
			wait("Connected " + root)
			send(report)
			_, id, _ = strings.Cut(strings.TrimSpace(ansi.Strip(wait(marker))), marker)
			if len(id) != 6 {
				t.Fatalf("could not read remote shell ID: %q", id)
			}

			send(report)
			wait(marker + id)
			send("\x02d")
			wait("Detached from " + root)
		})
		tree := string(local(t, ctx, "ps"))
		_, remoteTree, found := strings.Cut(tree, host+"\n")
		if !found || !strings.Contains(remoteTree, "shell/"+id+" (resume-root)  T") {
			t.Fatalf("independent remote shell is missing from its host root:\n%s", tree)
		}
		const missing = "ffffffffffffffffffffffffffffffff"
		parent := exec.CommandContext(ctx, binary, "shell", "--name", "resume-parent", "--no-profile", "--no-login")
		runOpenSSHTerminal(t, ctx, parent, func(send func(string), wait terminalWait) {
			wait("Connected here:resume-parent")
			// A managed shell can only hand off to its own remote children.
			send(quote(binary) + " attach " + id + "\n")
			wait("no live child matches")
			send(quote(binary) + " attach " + missing + "\n")
			wait("unknown shell ID")
			// The shell would otherwise inherit the rejected command's status.
			send("exit 0\n")
			wait("No shell attached.")
			send("exit\r")
		})
		// The original client account can resume the independent root by ID.
		runOpenSSHTerminal(t, ctx, command(ctx, "attach", id), func(send func(string), wait terminalWait) {
			wait("Connected " + root)
			send(report)
			wait(marker + id)
			send("exit\n")
		})
	})
	scenario("hosted shell uses explicit local selectors on its own daemon", func(t *testing.T, ctx context.Context) {
		root := host + ":scoped-root"
		cleanRemote(t, root)
		runOpenSSHTerminal(t, ctx, command(ctx, "sh", host, "--name", strings.TrimPrefix(root, host+":"), "--server", binary, "--no-profile", "--no-login"), func(send func(string), wait terminalWait) {
			wait("Connected " + root)
			send("sshore sh --name scoped-child --no-profile\n")
			wait("Connected " + host + ":scoped-child")
			send("export SSHORE_SCOPE_TEST=retained; printf 'SCOPE_%s=%s\\n' state \"$SSHORE_SCOPE_TEST\"\n")
			wait("SCOPE_state=retained")
			send("sshore d 3m\n")
			wait("Connected " + root)
			if tree := run(t, ctx, "sh", host, "--", "sshore ps"); !strings.HasPrefix(tree, "local\n") || !strings.Contains(tree, " (scoped-root)  R") || !strings.Contains(tree, " (scoped-child)  T") {
				t.Fatalf("hosted tree lost its local root or detached child:\n%s", tree)
			}
			send("sshore a scoped-child\n")
			wait("Connected " + host + ":scoped-child")
			send("printf 'SCOPE_%s=%s\\n' resumed \"$SSHORE_SCOPE_TEST\"\n")
			wait("SCOPE_resumed=retained")
			send("exit\n")
			wait("Connected " + root)
			send("exit\n")
			wait("Now in: calling terminal")
		})
	})
	scenario("remote child resumes after its parent exits", func(t *testing.T, ctx context.Context) {
		child := host + ":build"
		cleanRemote(t, child)
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		parent := exec.CommandContext(ctx, binary, "shell", "--name", "dev", "--no-profile", "--no-login")
		const report = "printf 'BUILD_%s=%s:%s:%.6s\\n' state \"$$\" \"$SSHORE_BUILD\" \"${SSHORE_TREE_ID#task:}\"\n"
		var recorded string
		runOpenSSHTerminal(t, ctx, parent, func(send func(string), wait terminalWait) {
			wait("Connected here:dev")
			send(quote(binary) + " shell " + host + " --name build --no-profile --no-login --server " + quote(binary) + " --ssh -F " + quote(config) + "\n")
			wait("Connected " + child)
			send("export SSHORE_BUILD=retained; " + report)
			const marker = "BUILD_state="
			line := ansi.Strip(wait(marker))
			_, state, _ := strings.Cut(strings.TrimSpace(line), marker)
			pid, remainder, _ := strings.Cut(state, ":")
			value, childID, _ := strings.Cut(remainder, ":")
			if number, err := strconv.Atoi(pid); err != nil || number <= 0 || value != "retained" || len(childID) != 6 {
				t.Fatalf("unexpected remote shell state: %q", line)
			}
			recorded = marker + state
			send("\x02d")
			wait("Connected here:dev")
			out, err := runTerminal(t, ctx, command(ctx, "attach", strings.TrimPrefix(child, host+":"), "--server", binary),
				func(_ func(string), wait terminalWait) { wait("from its parent") })
			if err == nil {
				t.Fatalf("a separate frontend entered the child before its parent exited:\n%s", out)
			}
			send(quote(binary) + " attach " + childID + "\n")
			wait("Connected " + child)
			send(report)
			wait(recorded)
			send("\x02d")
			wait("Connected here:dev")
			send(quote(binary) + " attach build\n")
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
		runOpenSSHTerminal(t, ctx, command(ctx, "attach", strings.TrimPrefix(child, host+":"), "--server", binary), func(send func(string), wait terminalWait) {
			wait("Connected " + child)
			send(report)
			wait(recorded)
			send("exit\n")
			wait("Exited " + child)
			wait("Now in: calling terminal")
		})
	})
	scenario("nested exit explains retained roots", func(t *testing.T, ctx context.Context) {
		build, child := host+":nested-build", host+":saved-child"
		cleanRemote(t, child)
		cleanRemote(t, build)
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		parent := exec.CommandContext(ctx, binary, "sh", "--name", "panel-dev", "--no-profile", "--no-login")
		runOpenSSHTerminal(t, ctx, parent, func(send func(string), wait terminalWait) {
			wait("Connected here:panel-dev")
			send(quote(binary) + " sh " + host + " --name nested-build --no-profile --no-login --server " + quote(binary) + " --ssh -F " + quote(config) + "\n")
			wait("Connected " + build)
			send(quote(binary) + " sh --name saved-child --no-profile --no-login\n")
			wait("Connected " + child)
			send("export SSHORE_PANEL_CHECK=kept\n\x02d")
			wait("Connected " + build)
			send("exit\n")
			wait("Exited " + build + " (code 0)")
			wait("Now in: here:panel-dev")
			wait("Pinned shells become independent roots")
			wait("(saved-child) · pinned indefinitely")
			wait("Connected here:panel-dev")
			send("exit\n")
			wait("Now in: local console")
			wait("No shell attached.")
			send("exit\r")
		})
		runOpenSSHTerminal(t, ctx, command(ctx, "attach", strings.TrimPrefix(child, host+":"), "--server", binary), func(send func(string), wait terminalWait) {
			wait("Connected " + child)
			send("printf 'SAVED_%s=%s\n' state \"$SSHORE_PANEL_CHECK\"\nexit\n")
			wait("SAVED_state=kept")
			wait("Exited " + child)
			wait("Now in: calling terminal")
			wait("No pinned shells remain from this subtree.")
		})
	})

	scenario("shutdown confirmation rejects a replacement task", func(t *testing.T, ctx context.Context) {
		// Confirmation applies to the tasks in the preview. Reusing a name
		// between preview and confirmation must leave the replacement alive.
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
			runOpenSSHTerminal(t, ctx, localCommand("sh", "--name", name, "--pin", "--no-profile"), func(send func(string), wait terminalWait) {
				wait("Connected " + endpoint)
				send("\x02d")
				wait("No shell attached.")
				send("exit\r")
			})
		}
		identity := func() string {
			t.Helper()
			for _, line := range strings.Split(string(local("list")), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[1] == "("+name+")" {
					return fields[0]
				}
			}
			t.Fatalf("missing shell %s", name)
			return ""
		}
		start()
		first := identity()
		var replacement string
		out, err := runTerminal(t, ctx, localCommand("kill", "--all"), func(send func(string), wait terminalWait) {
			wait(name)
			wait("[Y/n]", true)
			local("kill", first[:6])
			start()
			replacement = identity()
			send("y\n")
			wait("tasks changed; review again")
		})
		if err == nil || first == replacement || identity() != replacement {
			t.Fatalf("stale confirmation affected the replacement (%s → %s): %v\n%s", first, replacement, err, out)
		}
		if out, err := localCommand("kill", first).CombinedOutput(); err == nil {
			t.Fatalf("the retired task ID still accepts a kill: %s", out)
		}
		local("kill", replacement)
	})

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

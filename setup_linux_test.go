package main

import (
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

	"github.com/jay-waves/sshore/internal/files"
)

func TestRemoteHelpersRequireMatchingRelease(t *testing.T) {
	e := newEnv(t)
	binary, err := os.ReadFile(e.bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"mount-agent", "cp-agent"} {
		for _, tc := range []struct {
			name, installed string
			success         bool
		}{
			{"missing", "", false},
			{"matching", version, true},
			{"older", "0.0.1", false},
			{"newer", "999.0.0", false},
			{"unordered", "custom", false},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				home := t.TempDir()
				target := filepath.Join(home, "bin", "sshore")
				if tc.installed == version {
					os.MkdirAll(filepath.Dir(target), 0700)
					if err := os.WriteFile(target, binary, 0700); err != nil {
						t.Fatal(err)
					}
				} else if tc.installed != "" {
					writeRemoteVersionStub(t, target, tc.installed)
				}
				original, _ := os.ReadFile(target)
				tools := filepath.Join(home, "tools")
				os.Mkdir(tools, 0700)
				if err := os.WriteFile(filepath.Join(tools, "curl"), []byte("#!/bin/sh\necho offline >&2\nexit 22\n"), 0700); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "sh", "-c", remoteCommand(options{command: mode, server: target}))
				cmd.Env = append(e.env, "HOME="+home, "PATH="+tools+":/usr/bin:/bin", "TMPDIR="+home)
				var diagnostics bytes.Buffer
				cmd.Stderr = &diagnostics
				input, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				output, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { input.Close(); output.Close(); cmd.Process.Kill(); cmd.Wait() }()
				remoteHome, err := negotiateSetup(output, input, "fixture", false)
				if !tc.success {
					if err == nil || !strings.Contains(err.Error(), "sshore install") || !strings.Contains(err.Error(), version) {
						t.Fatalf("expected version and setup guidance: %v", err)
					}
					input.Close()
					output.Close()
					_ = cmd.Wait()
					after, _ := os.ReadFile(target)
					if !bytes.Equal(original, after) {
						t.Fatal("failed setup changed installation")
					}
					return
				}
				if err != nil {
					t.Fatalf("setup: %v", err)
				}
				if remoteHome != home {
					t.Fatalf("remote home = %q, want %q", remoteHome, home)
				}
				wire := &stdioPipe{Reader: output, Writer: input, closers: []io.Closer{input, output}}
				var data io.ReadWriteCloser = wire
				if mode == "mount-agent" {
					sendTest(t, wire, message{Type: "mount_data", SoftwareVersion: version})
					if reply := recv(t, wire); reply.Type != "mount_ready" {
						t.Fatalf("mount agent handshake: %+v", reply)
					}
				}
				client, err := files.NewSFTP(data, data)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.Stat(target); err != nil {
					t.Fatal(err)
				}
				client.Close()
				input.Close()
				output.Close()
				_ = cmd.Wait()
				installed, err := executableVersion(target)
				if err != nil || installed != version {
					t.Fatalf("installed=%s %v", installed, err)
				}
				if strings.Contains(diagnostics.String(), "DATA RACE") {
					t.Fatal(diagnostics.String())
				}
				if staged, _ := filepath.Glob(filepath.Join(home, "sshore-setup.*")); len(staged) != 0 {
					t.Fatalf("staging retained: %v", staged)
				}
				if records, err := hostedShells("health"); err != nil || len(records) != 0 {
					t.Fatalf("helper created a persistent shell: %+v %v", records, err)
				}
			})
		}
	}
}

// Exercise the application codec against the mount agent's narrower messages.
func TestMountAgentControlCompatibility(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan error, 1)
	go func() { done <- files.ServeAgent(a, version) }()
	defer func() {
		b.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("mount agent did not exit")
		}
	}()
	if err := writeMessage(b, message{Type: "mount_data", SoftwareVersion: version}); err != nil {
		t.Fatal(err)
	}
	reply, err := readMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply.Error, "sftp-server executable unavailable") {
		t.Skip(reply.Error)
	}
	if reply.Type != "mount_ready" || reply.SoftwareVersion != version {
		t.Fatalf("incompatible mount reply: %+v", reply)
	}
}

func runRemoteCommand(t *testing.T, home string, o options) (string, error) {
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

func writeRemoteVersionStub(t *testing.T, path, release string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo " + shellQuote("sshore "+release) + "; exit 0; fi\nprintf '<%s>\\n' \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func remoteServiceStubOutput(req remoteServiceRequest) string {
	return "<" + strings.Join(remoteServiceArgs(req), ">\n<") + ">\n"
}

func TestRemoteFallbackAndArgumentQuoting(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin", "sshore")
	writeRemoteVersionStub(t, bin, version)
	for _, mode := range []string{"bridge", "sessions", "new", "attach"} {
		o := options{server: "sshore", session: "main", list: mode == "sessions", command: mode}
		if mode == "new" {
			o.command = "launch"
		}
		out, err := runRemoteCommand(t, home, o)
		req := remoteServiceRequest{Operation: "shell", Mode: mode, Name: "main"}
		if mode == "sessions" {
			req = remoteServiceRequest{Operation: "inventory"}
		}
		want := remoteServiceStubOutput(req)
		if err != nil || out != want {
			t.Fatal(out, err)
		}
	}
	odd := filepath.Join(home, "server ' $(touch INJECTED)")
	if err := os.Rename(bin, odd); err != nil {
		t.Fatal(err)
	}
	name := "name ' $(touch INJECTED)"
	out, err := runRemoteCommand(t, home, options{server: odd, session: name})
	if err != nil || out != remoteServiceStubOutput(remoteServiceRequest{Operation: "shell", Mode: "bridge", Name: name}) {
		t.Fatal(out, err)
	}
	if _, err := os.Stat("INJECTED"); !os.IsNotExist(err) {
		t.Fatal("shell interpolation occurred", err)
	}
}

func TestRemoteServiceRejectsUnsupportedProtocol(t *testing.T) {
	for _, args := range [][]string{
		{remoteServiceCommand},
		{remoteServiceCommand, `{"protocol":"1","operation":"daemon-admin"}`},
		{remoteServiceCommand, `{"protocol":2,"release":"999.0.0","operation":"daemon-admin"}`},
		{remoteServiceCommand, `{"protocol":1,"operation":"daemon-admin"}`},
	} {
		if _, err := run(args); err == nil || !strings.Contains(err.Error(), "invalid remote service request") {
			t.Fatalf("%q: %v", args, err)
		}
	}
}

func TestRemoteKillRequiresMatchingRelease(t *testing.T) {
	for _, release := range []string{version, "0.0.1", "999.0.0", "custom", ""} {
		t.Run("version-"+release, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, "server ' name")
			if release != "" {
				writeRemoteVersionStub(t, bin, release)
			}
			out, err := runRemoteCommand(t, home, options{command: "kill", server: bin, session: "build"})
			if release == version {
				if err != nil || out != remoteServiceStubOutput(remoteServiceRequest{Operation: "kill-session", Name: "build"}) {
					t.Fatal(out, err)
				}
			} else if err == nil || !strings.Contains(out, "kill requires") {
				t.Fatal(out, err)
			}
			if strings.Contains(out, "Downloading") || strings.Contains(out, `"operation":"shell"`) {
				t.Fatal(out)
			}
		})
	}
}

func TestRenameRemoteRouting(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "server")
	writeRemoteVersionStub(t, bin, version)
	o := options{command: "rename", server: bin, session: "old", newName: "new"}
	out, err := runRemoteCommand(t, home, o)
	if err != nil || out != remoteServiceStubOutput(remoteServiceRequest{Operation: "rename-session", Name: "old", NewName: "new"}) {
		t.Fatal(out, err)
	}
	writeRemoteVersionStub(t, bin, "999.0.0")
	out, err = runRemoteCommand(t, home, o)
	if err == nil || strings.Contains(out, "Downloading") || !strings.Contains(out, "rename requires") {
		t.Fatal(out, err)
	}
}

func TestRemoteShellRejectsVersionMismatch(t *testing.T) {
	for _, command := range []string{"launch", "spawn", "attach"} {
		t.Run(command, func(t *testing.T) {
			home := t.TempDir()
			target := filepath.Join(home, "sshore")
			writeRemoteVersionStub(t, target, "0.0.1")
			out, err := runRemoteCommand(t, home, options{command: command, server: target})
			if err == nil || !strings.Contains(out, "sshore install") || !strings.Contains(out, target) {
				t.Fatalf("expected setup guidance: %q %v", out, err)
			}
		})
	}
}

func TestSetupAppliesPreparedRelease(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(t.TempDir(), "custom name")
	cmd := exec.Command(e.bin, "install", "--apply", "here:"+target)
	cmd.Env = e.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup: %v: %s", err, out)
	}
	if got, err := executableVersion(target); err != nil || got != version {
		t.Fatalf("installation: %q %v", got, err)
	}
}

func TestInstallWithRunningTasks(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("build")
	defer b.close()

	apply := func(target, answer string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, e.bin, "install", "--apply", "here:"+target)
		cmd.Env = e.env
		cmd.Stdin = strings.NewReader(answer)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := apply(e.bin, ""); err != nil || strings.Contains(out, "[Y/n]") {
		t.Fatalf("same release: %s %v", out, err)
	}
	target := filepath.Join(t.TempDir(), "sshore")
	writeRemoteVersionStub(t, target, "0.0.1")
	out, err := apply(target, "no\n")
	if err == nil || !strings.Contains(out, "build") || !strings.Contains(out, "cancelled") {
		t.Fatalf("declined upgrade: %s %v", out, err)
	}
	if got, err := executableVersion(target); err != nil || got != "0.0.1" {
		t.Fatalf("declined upgrade changed target: %q %v", got, err)
	}
	if out, err := apply(target, "kill\n"); err != nil || !strings.Contains(out, "build") {
		t.Fatalf("confirmed upgrade: %s %v", out, err)
	}
	waitSessionEnded(t, e, "build", pid)
	if got, err := executableVersion(target); err != nil || got != version {
		t.Fatalf("installed release: %q %v", got, err)
	}
}

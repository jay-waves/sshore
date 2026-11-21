package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/jay-waves/sshore/internal/proc"
)

func TestKillAllLocalSnapshot(t *testing.T) {
	e := newCLI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(input string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env, cmd.Stdin = e.env, strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, name := range []string{"one", "two"} {
		if out, err := run("", "fixture-launch", "here:", name, "--detach"); err != nil {
			t.Fatal(out, err)
		}
	}
	if out, err := run("no\n", "kill", "--all"); err == nil || !strings.Contains(out, "(one)") || !strings.Contains(out, "(two)") {
		t.Fatal(out, err)
	}
	rows := e.shells(t)
	if len(rows) != 2 {
		t.Fatalf("expected two shells: %+v", rows)
	}
	wait := waitFixtureDaemon(t, filepath.Join(e.dir, "state"))
	if out, err := run("\n", "kill", "--all"); err != nil {
		t.Fatal(out, err)
	}
	wait()
	if out, err := run("", "kill", "--all"); err != nil {
		t.Fatal(out, err)
	}
	if rows := e.shells(t); len(rows) != 0 {
		t.Fatalf("remaining shells: %+v", rows)
	}
}

func TestKillAllStopsIdleDaemon(t *testing.T) {
	e := newCLI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = e.env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	run("fixture-launch", "here:", "idle", "--detach")
	run("kill", "here:idle")
	wait := waitFixtureDaemon(t, filepath.Join(e.dir, "state"))
	run("kill", "--all")
	wait()
}

func TestKillAllDoesNotCancelReplacement(t *testing.T) {
	e := newCLI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = e.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		return string(out)
	}
	run("fixture-launch", "here:", "same", "--detach")
	cmd := exec.CommandContext(ctx, e.bin, "kill", "--all")
	cmd.Env = e.env
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); cmd.Process.Kill() }()
	reader := bufio.NewReader(output)
	var text strings.Builder
	for !strings.Contains(text.String(), "[Y/n] ") {
		b, err := reader.ReadByte()
		if err != nil {
			t.Fatal(text.String(), err)
		}
		text.WriteByte(b)
	}
	run("kill", "here:same")
	run("fixture-launch", "here:", "same", "--detach")
	io.WriteString(input, "\n")
	input.Close()
	tail, _ := io.ReadAll(reader)
	if err = cmd.Wait(); err == nil || !strings.Contains(string(tail), "tasks changed") {
		t.Fatalf("replacement accepted: %v %s", err, tail)
	}
	rows := e.shells(t)
	if len(rows) != 1 || rows[0].Name != "same" {
		t.Fatalf("replacement lost: %+v", rows)
	}
}

func waitFixtureDaemon(t *testing.T, state string) func() {
	t.Helper()
	pid := fixtureDaemon(t, state).ManagerPID
	start, err := proc.LiveIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			if current, err := proc.LiveIdentity(pid); err != nil || current != start {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("daemon %d still running after kill --all", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestForwardStartupShowsSSHFailure(t *testing.T) {
	e := newCLI(t)
	root := e.dir
	script := filepath.Join(root, "ssh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' 'Error: remote port forwarding failed for listen port 5173' >&2\nexit 255\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(e.bin, "forward", "work:5173", ".:7777")
	cmd.Env = append(e.env, "SSHORE_SOCKET=", "SSH_CONNECTION=", "SSHORE_STATE_DIR="+filepath.Join(root, "state"), "SSHORE_SSH="+script)
	e.env = cmd.Env
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "remote port forwarding failed for listen port 5173") || strings.Contains(string(out), "startup failed; see") {
		t.Fatalf("native cause hidden: %v %s", err, out)
	}
}

func TestLocalDaemonPreservesShellOnClientLoss(t *testing.T) {
	e := newCLI(t)
	state := filepath.Join(e.dir, "daemon-state")
	env := append(e.env, "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=", "SSHORE_TEST_VALUE=first")
	e.env = env
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	saved := make(chan struct{}, 1)
	start := func(args []string, environment []string) (*exec.Cmd, *os.File, <-chan string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = environment
		terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 160, Rows: 30})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			terminal.Close()
			if cmd.ProcessState == nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		})
		ready := make(chan struct{}, 1)
		done := make(chan string, 1)
		go func() {
			var b strings.Builder
			buf := make([]byte, 4096)
			announced := false
			for {
				n, err := terminal.Read(buf)
				b.Write(buf[:n])
				if !announced && strings.Contains(b.String(), "Connected here:") {
					announced = true
					ready <- struct{}{}
				}
				if strings.Contains(b.String(), "SAVED_first_VALUE") {
					select {
					case saved <- struct{}{}:
					default:
					}
				}
				if err != nil {
					done <- b.String()
					return
				}
			}
		}()
		select {
		case <-ready:
		case text := <-done:
			t.Fatalf("client ended: %s", text)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return cmd, terminal, done
	}
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		return out
	}
	first, terminal, _ := start([]string{"fixture-launch", "here:", "persistent"}, env)
	defer run("kill", "here:persistent")
	first.Process.Kill()
	first.Wait()
	terminal.Close()
	var rows []cliShell
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows = e.shells(t)
		if len(rows) == 1 && rows[0].State == "detached" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client loss did not preserve shell: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid := rows[0].PID
	second, terminal, done := start([]string{"attach", "here:" + rows[0].ShortID}, append(env, "SSHORE_TEST_VALUE=second"))
	io.WriteString(terminal, "printf 'SAVED_%s_VALUE\\n' \"$SSHORE_TEST_VALUE\"\n")
	select {
	case <-saved:
	case <-ctx.Done():
		t.Fatal("shell did not answer", ctx.Err())
	}
	io.WriteString(terminal, "\x02d")
	if err := second.Wait(); err != nil {
		t.Fatal(err)
	}
	terminal.Close()
	output := <-done
	if !strings.Contains(output, "SAVED_first_VALUE") {
		t.Fatalf("shell environment changed or input lost: %s", output)
	}
	rows = e.shells(t)
	if len(rows) != 1 || rows[0].PID != pid || rows[0].State != "detached" {
		t.Fatalf("shell identity changed: %+v", rows)
	}
}

func TestLocalNoLoginStartup(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	e := newCLI(t)
	home := t.TempDir()
	marker := filepath.Join(home, "startup")
	quotedMarker := "'" + strings.ReplaceAll(marker, "'", "'\"'\"'") + "'"
	for file, value := range map[string]string{".bash_profile": "login", ".bashrc": "interactive"} {
		if err := os.WriteFile(filepath.Join(home, file), []byte("printf "+value+" > "+quotedMarker+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	env := append(e.env, "HOME="+home, "SHELL="+bash, "SSHORE_SOCKET=", "SSHORE_STATE_DIR="+filepath.Join(e.dir, "local-state"))
	e.env = env
	run := func(args ...string) {
		t.Helper()
		c := exec.Command(e.bin, args...)
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	run("fixture-launch", "here:", "startup", "--no-login", "--detach")
	defer run("kill", "here:startup")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			if string(data) != "interactive" {
				t.Fatalf("wrong startup: %q", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

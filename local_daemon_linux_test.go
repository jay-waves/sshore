//go:build linux && sshoretest

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestLocalDaemonPreservesShellOnClientLoss(t *testing.T) {
	e := newEnv(t)
	state := filepath.Join(e.dir, "daemon-state")
	env := append(e.env, "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=", "SSHORE_TEST_VALUE=first")
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
				if !announced && strings.Contains(b.String(), "Connected local /") {
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
	first, terminal, _ := start([]string{"shell", "--name", "persistent"}, env)
	defer run("cancel", "--local", "persistent")
	first.Process.Kill()
	first.Wait()
	terminal.Close()
	var rows []localShellView
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := json.Unmarshal(run("list", "--local", "--json"), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].State == "detached" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client loss did not preserve shell: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid := rows[0].PID
	second, terminal, done := start([]string{"attach", "--local", "persistent"}, append(env, "SSHORE_TEST_VALUE=second"))
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
	if err := json.Unmarshal(run("list", "--local", "--json"), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PID != pid || rows[0].State != "detached" {
		t.Fatalf("shell identity changed: %+v", rows)
	}
}

func TestLocalDaemonOwnsForwardTransports(t *testing.T) {
	e := newEnv(t)
	state := filepath.Join(e.dir, "shared-forward")
	t.Cleanup(func() {
		deadline := time.Now().Add(35 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(state, "daemon", "local.json")); os.IsNotExist(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("local daemon survived idle shutdown after all tasks ended")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	env := append(e.env, "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=", "SSHORE_SSH=/missing/initial-ssh")
	run := func(environment []string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = environment
		cmd.Dir = e.dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		return string(out)
	}
	run(env, "shell", "--name", "keeper", "--detach")
	defer run(env, "cancel", "--local", "keeper")
	raw, err := os.ReadFile(filepath.Join(state, "daemon", "local.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manager localSessionRecord
	if err = json.Unmarshal(raw, &manager); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		observation := filepath.Join(e.dir, name+"-parent")
		stub := filepath.Join(e.dir, name+"-ssh")
		script := "#!/bin/sh\nprintf '%s\\n%s\\n' \"$PPID\" \"$PWD\" > " + shellQuote(observation) + "\nprintf 'sshore-forward-ready\\n'\nexec cat >/dev/null\n"
		if err = os.WriteFile(stub, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		run(append(env, "SSHORE_SSH="+stub), "forward", "8080", "fixture:3000", "--name", name)
		defer run(env, "cancel", "forward/"+name)
		raw, err = os.ReadFile(observation)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != fmt.Sprintf("%d\n%s\n", manager.ManagerPID, e.dir) {
			t.Fatalf("forward has a wrapper process or lost cwd: %s", raw)
		}
	}
}

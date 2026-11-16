//go:build sshoretest

package tests_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/jay-waves/sshore/internal/tmux"
)

func TestBashStartupFiles(t *testing.T) {
	path, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	home := t.TempDir()
	for file, value := range map[string]string{".bash_profile": "LOGIN", ".bashrc": "INTERACTIVE"} {
		if err := os.WriteFile(filepath.Join(home, file), []byte("export SSHORE_STARTUP_TEST="+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, noLogin := range []bool{false, true} {
		args, err := tmux.StartupArgs(path, noLogin, false)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, path, append(args, "-c", `printf '\nSTARTUP:%s\n' "$SSHORE_STARTUP_TEST"`)...)
		cmd.Env = append(os.Environ(), "HOME="+home, "SSHORE_STARTUP_TEST=", "TERM=dumb")
		out, err := cmd.CombinedOutput()
		cancel()
		want := "STARTUP:LOGIN"
		if noLogin {
			want = "STARTUP:INTERACTIVE"
		}
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("noLogin=%v: %v %s", noLogin, err, out)
		}
	}
}

func TestNoProfileStartupFiles(t *testing.T) {
	for _, name := range []string{"bash", "sh"} {
		t.Run(name, func(t *testing.T) {
			path, err := exec.LookPath(name)
			if err != nil {
				t.Skip(err)
			}
			home := t.TempDir()
			for _, file := range []string{".bash_profile", ".bashrc", ".profile", "env-hook"} {
				if err := os.WriteFile(filepath.Join(home, file), []byte("export SSHORE_PROFILE_RAN=yes\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args, err := tmux.StartupArgs(path, false, true)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, path, append(args, "-c", `printf '\nRESULT:%s:%s\n' "$SSHORE_PROFILE_RAN" "$SSHORE_INHERITED"`)...)
			source := append(os.Environ(), "HOME="+home, "TERM=dumb", "SSHORE_PROFILE_RAN=", "SSHORE_INHERITED=keep", "ENV="+filepath.Join(home, "env-hook"))
			cmd.Env = tmux.StartupEnv(path, source, true)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "RESULT::keep") {
				t.Fatalf("%v %s", err, out)
			}
			if source[len(source)-1] != "ENV="+filepath.Join(home, "env-hook") {
				t.Fatal("parent environment mutated")
			}
		})
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
	first, terminal, _ := start([]string{"launch", "--local", "persistent"}, env)
	defer run("cancel", "--local", "persistent")
	first.Process.Kill()
	first.Wait()
	terminal.Close()
	var rows []cliShell
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

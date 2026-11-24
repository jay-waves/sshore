//go:build integration

package tests_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	"golang.org/x/sys/windows"
)

// The frontend runs inside the harness ConPTY's job. Start the real account
// daemon outside that job so its lifetime can span two frontend processes.
func windowsClient(t *testing.T) (context.Context, func(...string) *exec.Cmd) {
	t.Helper()
	binary := os.Getenv("SSHORE_TEST_WINDOWS_CLIENT")
	if binary == "" {
		t.Skip("set SSHORE_TEST_WINDOWS_CLIENT to the built Windows executable on an isolated runner")
	}
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SHELL", filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	command := func(args ...string) *exec.Cmd { return exec.CommandContext(ctx, binary, args...) }
	daemonCtx, stopDaemon := context.WithTimeout(context.Background(), 75*time.Second)
	t.Cleanup(stopDaemon)
	daemon := exec.CommandContext(daemonCtx, binary, "daemon")
	stderr, err := daemon.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	ready := false
	t.Cleanup(func() {
		if ready {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			stop := exec.CommandContext(ctx, binary, "kill", "--all")
			stop.Stdin = strings.NewReader("kill\n")
			if out, err := stop.CombinedOutput(); err != nil {
				t.Errorf("stop fixture daemon: %v\n%s", err, out)
			}
		}
		_ = daemon.Process.Kill()
		_ = daemon.Wait()
	})
	reader := bufio.NewReader(stderr)
	line, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "Daemon running." {
		t.Fatalf("fixture daemon did not start: %q (%v)", line, err)
	}
	ready = true
	go func() { _, _ = io.Copy(os.Stderr, reader) }()
	return ctx, command
}

func runWindowsTerminal(t *testing.T, ctx context.Context, cmd *exec.Cmd, run func(func(string), terminalWait)) {
	t.Helper()
	terminal, err := pty.Start(pty.Config{
		Path: cmd.Path, Args: cmd.Args[1:], Env: cmd.Environ(), Dir: cmd.Dir,
		Size: pty.Size{Cols: 180, Rows: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	stop := context.AfterFunc(ctx, func() { _ = terminal.Close() })
	defer stop()
	transcript := terminalDialogue(t, terminal, func(send func(string), wait terminalWait) {
		// ConPTY paints text with cursor movement and can omit the final newline.
		run(send, func(marker string, _ ...bool) string { return wait(marker, true) })
	})
	if err := terminal.Wait(); err != nil {
		t.Fatalf("Windows terminal client: %v\n%s", err, transcript)
	}
}

func windowsRun(t *testing.T, cmd *exec.Cmd) []byte {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", cmd.Args, err, out)
	}
	return out
}

func TestWindowsConPTY(t *testing.T) {
	ctx, command := windowsClient(t)
	directory := t.TempDir()
	const endpoint = "here:conpty"
	runWindowsTerminal(t, ctx, command("sh", endpoint, "--pin", "--no-profile"), func(send func(string), wait terminalWait) {
		wait("Connected " + endpoint)
		send("set \"SSHORE_RESUME=retained\"\r")
		send("cd /d \"" + directory + "\"\r")
		send("echo WIN_READY=%SSHORE_RESUME%\r")
		wait("WIN_READY=retained")
		send("\x02d")
		wait("Detached from " + endpoint)
		wait("No shell attached.")
		wait("sshore on local>")
		send("exit\r")
	})
	runWindowsTerminal(t, ctx, command("attach", endpoint), func(send func(string), wait terminalWait) {
		wait("WIN_READY=retained")
		send("echo WIN_RESUMED=%SSHORE_RESUME%:%CD%\r")
		wait("WIN_RESUMED=retained:" + directory)
		send("exit\r")
		wait("No shell attached.")
		wait("sshore on local>")
		send("exit\r")
	})
	var inventory struct {
		LocalShells []struct{ Name string } `json:"local_shells"`
	}
	if err := json.Unmarshal(windowsRun(t, command("list", "--json")), &inventory); err != nil {
		t.Fatal(err)
	}
	for _, task := range inventory.LocalShells {
		if task.Name == "conpty" {
			t.Fatal("exited shell still registered")
		}
	}
}

func TestWindowsWinFsp(t *testing.T) {
	server := os.Getenv("SSHORE_TEST_WSL_SERVER")
	if server == "" {
		t.Skip("set SSHORE_TEST_WSL_SERVER to the Linux build on the WSL test runner")
	}
	config := startWindowsSSH(t, server, t.TempDir())
	ctx, command := windowsClient(t)
	ssh := func(args ...string) *exec.Cmd { return command(append(args, "--", "-F", config)...) }
	windowsRun(t, ssh("exec", "winfsp-fixture", "printf original > share/target"))
	start := string(windowsRun(t, ssh("mount", "winfsp-fixture:share", "here:")))
	_, suffix, _ := strings.Cut(start, "Started mount/")
	name, _, _ := strings.Cut(suffix, ":")
	if name == "" {
		t.Fatalf("missing registered mount: %s", start)
	}
	var inventory struct {
		Resources []struct{ ID, Mountpoint string }
	}
	if err := json.Unmarshal(windowsRun(t, command("list", "--json")), &inventory); err != nil {
		t.Fatal(err)
	}
	task := "mount/" + name
	var root string
	for _, resource := range inventory.Resources {
		if resource.ID == task {
			root = resource.Mountpoint + `\`
			break
		}
	}
	root = strings.ToUpper(root)
	if len(root) != 3 || root[0] < 'A' || root[0] > 'Z' || root[1] != ':' {
		t.Fatalf("missing mounted drive for %s: %+v", task, inventory.Resources)
	}
	target, temporary := filepath.Join(root, "target"), filepath.Join(root, "save.tmp")
	requireFile(t, target, "original")
	if err := os.WriteFile(temporary, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, target); err != nil {
		t.Fatal(err)
	}
	requireFile(t, target, "replacement")
	if got := windowsRun(t, ssh("exec", "winfsp-fixture", "cat share/target")); string(got) != "replacement" {
		t.Fatalf("remote file after replace: %q", got)
	}
	if suite := os.Getenv("SSHORE_TEST_WINFSP_SUITE"); suite != "" {
		dir := filepath.Join(root, "official-suite")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		cases := []string{"rdwr_cached_test", "rdwr_cached_append_test", "querydir_test"}
		cmd := exec.CommandContext(ctx, suite, append([]string{"--external", "--resilient", "--tap"}, cases...)...)
		cmd.Dir = dir
		out := string(windowsRun(t, cmd))
		t.Log(out)
		for _, name := range cases {
			if !strings.Contains(strings.ReplaceAll(out, "\r\n", "\n"), " "+name+"\n") {
				t.Fatalf("WinFsp did not report selected case %s", name)
			}
		}
	}
	windowsRun(t, command("kill", task))
	drives, err := windows.GetLogicalDrives()
	if err != nil || drives&(1<<uint(root[0]-'A')) != 0 {
		t.Fatalf("drive %s remained after unmount: %v", root, err)
	}
}

func startWindowsSSH(t *testing.T, server, dir string) string {
	t.Helper()
	ctx, stop := context.WithTimeout(context.Background(), 80*time.Second)
	t.Cleanup(stop)
	wsl := func(args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "wsl.exe", append([]string{"--distribution", "Ubuntu-24.04", "--user", "root", "--exec"}, args...)...)
	}
	toLinux := func(path string) string {
		t.Helper()
		return strings.TrimSpace(string(windowsRun(t, wsl("wslpath", "-a", path))))
	}
	script, err := filepath.Abs("windows_ssh.sh")
	if err != nil {
		t.Fatal(err)
	}
	sshd := wsl("/bin/sh", toLinux(script), toLinux(server), toLinux(dir))
	sshd.Stderr = os.Stderr
	if err := sshd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sshd.Wait() }()
	t.Cleanup(func() { stop(); <-done })
	ready, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		conn, err := net.DialTimeout("tcp4", "127.0.0.1:2222", 250*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case err := <-done:
			done <- err // Leave the result for fixture cleanup.
			t.Fatalf("WSL sshd stopped: %v", err)
		case <-ready.Done():
			t.Fatalf("WSL sshd did not listen: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	config := filepath.Join(dir, "ssh_config")
	content := fmt.Sprintf(`Host winfsp-fixture
    HostName 127.0.0.1
    Port 2222
    User sshore-winfsp
    BatchMode yes
    StrictHostKeyChecking yes
    IdentityFile "%s"
    UserKnownHostsFile "%s"
`, filepath.ToSlash(filepath.Join(dir, "identity")), filepath.ToSlash(filepath.Join(dir, "known_hosts")))
	if err := os.WriteFile(config, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return config
}

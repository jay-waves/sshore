//go:build integration

package tests_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("SSHORE_PROGRAM_FIXTURE") == "1" {
		name := strings.ToLower(filepath.Base(os.Args[0]))
		if name == "vim.exe" || name == "less.exe" {
			args, _ := json.Marshal(os.Args[1:])
			fmt.Printf("PROGRAM_ARGS=%s\n", args)
			return
		}
	}
	os.Exit(m.Run())
}

func TestWindowsNewReview(t *testing.T) {
	binary := os.Getenv("SSHORE_TEST_WINDOWS_CLIENT")
	if binary == "" {
		t.Skip("set SSHORE_TEST_WINDOWS_CLIENT to a built executable")
	}
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", state)
	t.Setenv("SSHORE_DISABLED", "")
	ctx := t.Context()
	cmd := exec.CommandContext(ctx, binary, "new", "--", "vim", "a b.txt")
	out, err := runWindowsTerminal(t, ctx, cmd, func(send func(string), wait terminalWait) {
		wait(`["vim","a b.txt"]`)
		wait("Esc Cancel", true)
		send("\x1b[A")
		send("e")
		wait("Arguments>", true)
		send("\x01\x0b[\"vim\",null]\r")
		wait("Use string arguments without NUL")
		wait("Arguments>", true)
		send("\x01\x0b[\"vim\",\"c d.txt\",\"\"]\r")
		wait("Esc Cancel", true)
		send("\x1b")
	})
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if !strings.Contains(out, `["vim","c d.txt",""]`) {
		t.Fatalf("edited arguments missing:\n%s", out)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("canceling review changed task state: %v", err)
	}
}

func confirmNewTerminal(send func(string), wait terminalWait) {
	wait("Esc Cancel", true)
	send("\r")
}

func TestWindowsProgramArguments(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(program)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range []string{"vim.exe", "less.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), binary, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SSHORE_PROGRAM_FIXTURE", "1")
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	t.Setenv("SSHORE_TREE_ID", "")
	t.Setenv("SSHORE_SOCKET", "")
	ctx, command := windowsClient(t)
	t.Run("new without delimiter opens a shell", func(t *testing.T) {
		out, err := runWindowsTerminal(t, ctx, command("new", "--name", "default-shell"), func(send func(string), wait terminalWait) {
			confirmNewTerminal(send, wait)
			wait("Connected here:default-shell")
			send("echo DEFAULT_SHELL_READY\r")
			wait("DEFAULT_SHELL_READY")
			send("exit\r")
			wait("sshore on local>", true)
			send("exit\r")
		})
		if err != nil {
			t.Fatalf("default shell: %v\n%s", err, out)
		}
	})
	for _, tc := range []struct {
		name, program string
		args          []string
	}{
		{"vim one file", "vim.exe", []string{"xxx"}},
		{"vim spaced file", "vim.exe", []string{"xxx yyy"}},
		{"vim two files", "vim.exe", []string{"xxx", "yyy"}},
		{"less one file", "less.exe", []string{"xxx"}},
		{"less spaced file", "less.exe", []string{"xxx yyy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"new", "--", filepath.Join(dir, tc.program)}, tc.args...)
			want, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			out, err := runWindowsTerminal(t, ctx, command(args...), func(send func(string), wait terminalWait) {
				confirmNewTerminal(send, wait)
				wait("PROGRAM_ARGS=" + string(want))
				wait("Now in: calling terminal")
			})
			if err != nil {
				t.Fatalf("program arguments: %v\n%s", err, out)
			}
		})
	}
}

func TestWindowsRunFixture(t *testing.T) {
	if os.Getenv("SSHORE_RUN_FIXTURE") != "1" {
		t.Skip("run fixture")
	}
	want := []string{"", "space value", `quote"value`, `back\slash`, "--ssh", "--"}
	found := false
	for i, arg := range os.Args {
		if arg == "fixture-args" {
			found = true
			if !reflect.DeepEqual(os.Args[i+1:], want) {
				t.Fatalf("arguments = %#v, want %#v", os.Args[i+1:], want)
			}
			break
		}
	}
	if !found {
		t.Fatal("fixture arguments missing")
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("RESUMED=%s\n", strings.TrimSpace(line))
}

// Exercise task identity through separate invocations of the real client.
func TestWindowsRunLifecycle(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_RUN_FIXTURE", "1")
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	t.Setenv("SSHORE_TREE_ID", "")
	t.Setenv("SSHORE_SOCKET", "")
	ctx, command := windowsClient(t)
	start := func() *exec.Cmd {
		return command("new", "--name", "lifecycle", "--pin", "--", program, "-test.run=^TestWindowsRunFixture$", "--", "fixture-args", "", "space value", `quote"value`, `back\slash`, "--ssh", "--")
	}
	missing := filepath.Join(t.TempDir(), "missing.exe")
	out, err := runWindowsTerminal(t, ctx, command("new", "--name", "lifecycle", "--", missing), confirmNewTerminal)
	if err == nil {
		t.Fatalf("missing executable succeeded:\n%s", out)
	}
	if tree := string(windowsRun(t, command("ps"))); strings.Contains(tree, "(lifecycle)") {
		t.Fatalf("failed launch left a task registered:\n%s", tree)
	}
	out, err = runWindowsTerminal(t, ctx, start(), func(send func(string), wait terminalWait) {
		confirmNewTerminal(send, wait)
		wait("Connected here:lifecycle")
		send("\x02d")
		wait("Detached from here:lifecycle")
		wait("sshore on local>", true)
		send("exit\r")
	})
	if err != nil {
		t.Fatalf("detach: %v\n%s", err, out)
	}
	tree := string(windowsRun(t, command("ps")))
	_, entry, found := strings.Cut(tree, "run/")
	if !found || !strings.Contains(entry, " (lifecycle)  detached") {
		t.Fatalf("detached task missing:\n%s", tree)
	}
	id, _, _ := strings.Cut(entry, " ")
	if len(id) != 6 {
		t.Fatalf("unexpected task ID %q in:\n%s", id, tree)
	}
	detail := string(windowsRun(t, command("list")))
	programDetail := program
	if strings.ContainsAny(programDetail, " \"'`") {
		programDetail = strconv.Quote(programDetail)
	}
	wantDetail := programDetail + ` -test.run=^TestWindowsRunFixture$ -- fixture-args "" "space value" "quote\"value" back\slash --ssh --`
	if !strings.Contains(detail, "pinned "+wantDetail) {
		t.Fatalf("list did not show the pinned program and its arguments:\n%s", detail)
	}
	if out, err := command("kill", "unknown-remote-host").CombinedOutput(); err == nil || !strings.Contains(string(out), "unknown task") {
		t.Fatalf("kill without --all should only look for a named task: %v\n%s", err, out)
	}

	out, err = runWindowsTerminal(t, ctx, start(), confirmNewTerminal)
	if err == nil || !strings.Contains(out, "already") {
		t.Fatalf("duplicate name should fail: %v\n%s", err, out)
	}
	tree = string(windowsRun(t, command("ps")))
	if strings.Count(tree, "(lifecycle)") != 1 || !strings.Contains(tree, "run/"+id) {
		t.Fatalf("duplicate changed the existing task:\n%s", tree)
	}

	out, err = runWindowsTerminal(t, ctx, command("attach", "lifecycle"), func(send func(string), wait terminalWait) {
		wait("Connected here:lifecycle")
		send("\x02d")
		wait("Detached from here:lifecycle")
		wait("sshore on local>", true)
		send("exit\r")
	})
	if err != nil {
		t.Fatalf("reattach by name: %v\n%s", err, out)
	}
	if tree = string(windowsRun(t, command("ps"))); !strings.Contains(tree, "run/"+id) {
		t.Fatalf("name reattach changed the task ID:\n%s", tree)
	}

	out, err = runWindowsTerminal(t, ctx, command("attach", id), func(send func(string), wait terminalWait) {
		wait("Connected here:lifecycle")
		send("kept\r")
		wait("RESUMED=kept")
		wait("sshore on local>", true)
		send("exit\r")
	})
	if err != nil {
		t.Fatalf("reattach: %v\n%s", err, out)
	}
	if tree = string(windowsRun(t, command("ps"))); strings.Contains(tree, "(lifecycle)") {
		t.Fatalf("finished task remains registered:\n%s", tree)
	}

	out, err = runWindowsTerminal(t, ctx, start(), func(send func(string), wait terminalWait) {
		confirmNewTerminal(send, wait)
		wait("Connected here:lifecycle")
		send("\x02d")
		wait("Detached from here:lifecycle")
		wait("sshore on local>", true)
		send("exit\r")
	})
	if err != nil {
		t.Fatalf("detach before kill: %v\n%s", err, out)
	}
	windowsRun(t, command("kill", "lifecycle"))
	if tree = string(windowsRun(t, command("ps"))); strings.Contains(tree, "(lifecycle)") {
		t.Fatalf("killed task remains registered:\n%s", tree)
	}
}

//go:build linux && sshoretest

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCancelAllLocalSnapshot(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	env := append(e.env, "SSHORE_STATE_DIR="+filepath.Join(e.dir, "batch-state"), "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=")
	run := func(input string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env, cmd.Stdin = env, strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, name := range []string{"one", "two"} {
		if out, err := run("", "shell", "--name", name, "--detach"); err != nil {
			t.Fatal(out, err)
		}
		t.Cleanup(func() { run("", "cancel", "--local", name) })
	}
	if out, err := run("no\n", "cancel", "--all"); err == nil || !strings.Contains(out, "shell/one") || !strings.Contains(out, "shell/two") {
		t.Fatal(out, err)
	}
	out, err := run("", "list", "--local", "--json")
	var rows []localShellView
	if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 2 {
		t.Fatal(out, err)
	}
	if out, err = run("cancel\n", "cancel", "--all"); err != nil {
		t.Fatal(out, err)
	}
	out, err = run("", "list", "--local", "--json")
	if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 0 {
		t.Fatal(out, err)
	}
}

func TestCancelAllRemoteScope(t *testing.T) {
	e := newEnv(t)
	first, firstPID := e.attach("one")
	second, secondPID := e.attach("two")
	defer first.close()
	defer second.close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wrapper := filepath.Join(e.dir, "ssh-batch")
	script := "#!/bin/sh\nexport SSHORE_STATE_DIR=" + shellQuote(filepath.Join(e.dir, "state")) + "\nfor last; do :; done\nexec /bin/sh -c \"$last\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	env := append(e.env, "SSHORE_SSH="+wrapper, "SSHORE_SOCKET=", "SSHORE_STATE_DIR="+filepath.Join(e.dir, "local-state"))
	run := func(input string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("", "shell", "--name", "keep", "--detach"); err != nil {
		t.Fatal(out, err)
	}
	defer run("", "cancel", "--local", "keep")
	if out, err := run("cancel\n", "cancel", "fixture", "--all", "--server", e.bin); err != nil {
		t.Fatal(out, err)
	}
	waitSessionEnded(t, e, "one", firstPID)
	waitSessionEnded(t, e, "two", secondPID)
	out, err := run("", "list", "--local", "--json")
	var rows []localShellView
	if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].Name != "keep" {
		t.Fatal(out, err)
	}
}

func TestCancelAllDoesNotCancelReplacement(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	env := append(e.env, "SSHORE_STATE_DIR="+filepath.Join(e.dir, "replacement-state"), "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		return string(out)
	}
	run("shell", "--name", "same", "--detach")
	defer run("cancel", "--local", "same")
	cmd := exec.CommandContext(ctx, e.bin, "cancel", "--all")
	cmd.Env = env
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
	for !strings.Contains(text.String(), "Type cancel: ") {
		b, err := reader.ReadByte()
		if err != nil {
			t.Fatal(text.String(), err)
		}
		text.WriteByte(b)
	}
	run("cancel", "--local", "same")
	run("shell", "--name", "same", "--detach")
	io.WriteString(input, "cancel\n")
	input.Close()
	tail, _ := io.ReadAll(reader)
	if err = cmd.Wait(); err == nil || !strings.Contains(string(tail), "task changed") {
		t.Fatalf("replacement accepted: %v %s", err, tail)
	}
	out := run("list", "--local", "--json")
	var rows []localShellView
	if json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].Name != "same" {
		t.Fatal(out)
	}
}

package tests_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/sshtest"
)

func TestCopyCLI(t *testing.T) {
	e := newCLI(t)
	root := e.dir
	wrapper := copySSH(t, e)
	var err error
	source := filepath.Join(root, "source")
	remote := filepath.Join(root, "remote")
	target := filepath.Join(root, "target")
	if err = os.WriteFile(source, []byte("direct cp"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Dir = root
		cmd.Env = append(e.env, "SSHORE_SSH="+wrapper, "SSHORE_TEST_BINARY="+e.bin)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Log(string(output))
		}
		return err
	}
	if err = run("cp", "origin:"+source, "work:"+remote); err != nil {
		t.Fatal(err)
	}
	if err = run("cp", "work:"+remote, "origin:"+target); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "direct cp" {
		t.Fatal(string(data), err)
	}
	if err = run("cp", source, "work:"+remote); err == nil {
		t.Fatal("overwrite accepted")
	}
	if err = run("cp", source, "work:relative-copy"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "relative-copy")); err != nil || string(got) != "direct cp" {
		t.Fatal("relative remote path", string(got), err)
	}
	streamTarget := filepath.Join(root, "streamed")
	data = bytes.Repeat([]byte("pipe data\x00\xff\n"), 10000)
	for _, args := range [][]string{{"cp", "-", "work:" + streamTarget}, {"cp", "work:" + streamTarget, "-"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Dir = root
		cmd.Env = append(e.env, "SSHORE_SSH="+wrapper, "SSHORE_TEST_BINARY="+e.bin)
		cmd.Stdin = bytes.NewReader(data)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		cancel()
		if err != nil {
			t.Fatal(err, stderr.String())
		}
		if args[1] == "-" && len(out) != 0 {
			t.Fatal("upload wrote to stdout")
		}
		if args[2] == "-" && !bytes.Equal(out, data) {
			t.Fatal("stdout bytes corrupted")
		}
	}
	for _, old := range []string{"upload", "download", "publish", "subscribe"} {
		if err = run(old, "--help"); err == nil {
			t.Fatal("old command remains", old)
		}
	}
}

func TestCopyPreflight(t *testing.T) {
	e := newCLI(t)
	remote := filepath.Join(e.dir, "remote")
	if err := os.Mkdir(remote, 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := copySSH(t, e)
	copy := func(sources ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		args := append([]string{"cp"}, sources...)
		cmd := exec.CommandContext(ctx, e.bin, append(args, "work:"+remote)...)
		cmd.Env = append(e.env, "SSHORE_SSH="+wrapper)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Log(string(out))
		}
		return err
	}
	local := t.TempDir()
	a := filepath.Join(local, "a")
	if err := os.WriteFile(a, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copy(a); err != nil {
		t.Fatal("fixture cannot copy a valid source", err)
	}
	if err := os.Remove(filepath.Join(remote, "a")); err != nil {
		t.Fatal(err)
	}
	if err := copy(a, filepath.Join(local, "missing")); err == nil {
		t.Fatal("accepted missing source")
	}
	if _, err := os.Stat(filepath.Join(remote, "a")); !os.IsNotExist(err) {
		t.Fatal("wrote before preflight completed")
	}
	if err := copy(a, a); err == nil {
		t.Fatal("accepted duplicate target")
	}
	if _, err := os.Stat(filepath.Join(remote, "a")); !os.IsNotExist(err) {
		t.Fatal("duplicate batch partly written")
	}
	if err := copy(local); err == nil {
		t.Fatal("accepted directory source")
	}
}

func copySSH(t *testing.T, e *cliFixture) string {
	t.Helper()
	wrapper := filepath.Join(e.dir, "ssh")
	script := "#!/bin/sh\nexec \"$SSHORE_TEST_BINARY\" bootstrap \"$SSHORE_TEST_BINARY\" cp-agent ''\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	wrapper = sshtest.Wrap(t, wrapper, append(e.env, "SSHORE_TEST_BINARY="+e.bin))
	return wrapper
}

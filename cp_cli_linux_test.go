package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

type copyTestStdio struct{}

func (copyTestStdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (copyTestStdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (copyTestStdio) Close() error                { return nil }

func TestCopySFTPProcess(t *testing.T) {
	if os.Getenv("SSHORE_TEST_COPY_SUBSYSTEM") != "1" {
		return
	}
	server, err := sftp.NewServer(copyTestStdio{})
	if err != nil {
		os.Exit(2)
	}
	err = server.Serve()
	if err != nil && err != io.EOF {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestCopyCLI(t *testing.T) {
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "ssh")
	script := "#!/bin/sh\nexport SSHORE_TEST_COPY_SUBSYSTEM=1\nexec " + shellQuote(executable) + " -test.run=^TestCopySFTPProcess$\n"
	if err = os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	remote := filepath.Join(root, "remote")
	target := filepath.Join(root, "target")
	if err = os.WriteFile(source, []byte("direct cp"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, testBinary(t), args...)
		cmd.Env = append(os.Environ(), "SSHORE_SOCKET=", "SSHORE_DISABLED=", "SSHORE_SSH="+wrapper)
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
	streamTarget := filepath.Join(root, "streamed")
	data = bytes.Repeat([]byte("pipe data\x00\xff\n"), 10000)
	for _, args := range [][]string{{"cp", "-", "work:" + streamTarget}, {"cp", "work:" + streamTarget, "-"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, testBinary(t), args...)
		cmd.Env = append(os.Environ(), "SSHORE_SOCKET=", "SSHORE_DISABLED=", "SSHORE_SSH="+wrapper)
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

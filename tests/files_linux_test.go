//go:build sshoretest

package tests_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/files"
)

func TestCopyCLI(t *testing.T) {
	e := newCLI(t)
	root := e.dir
	wrapper := filepath.Join(root, "ssh")
	script := "#!/bin/sh\nexec \"$SSHORE_TEST_BINARY\" bootstrap \"$SSHORE_TEST_BINARY\" cp-agent ''\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
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

func TestMountApprovalSourceGate(t *testing.T) {
	a, b := net.Pipe()
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	defer b.Close()
	called := make(chan files.MountApproval, 1)
	done := make(chan error, 1)
	go func() {
		done <- files.ServeAgent(a, "1.2.3", func(source files.MountApproval) error { called <- source; return fmt.Errorf("source changed") })
	}()
	approval := &files.MountApproval{Session: "work", PID: 123, ID: strings.Repeat("a", 32)}
	if _, _, _, err := files.OpenMux(b, "1.2.3", approval); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("gate bypassed: %v", err)
	}
	if got := <-called; got != *approval {
		t.Fatal(got)
	}
	if err := <-done; err == nil {
		t.Fatal("agent accepted failed approval")
	}
}

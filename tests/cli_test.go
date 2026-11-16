//go:build sshoretest

package tests_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var cliBuild struct {
	sync.Once
	directory, binary string
	err               error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if cliBuild.directory != "" {
		os.RemoveAll(cliBuild.directory)
	}
	os.Exit(code)
}

func testBinary(t *testing.T) string {
	t.Helper()
	cliBuild.Do(func() {
		cliBuild.directory, cliBuild.err = os.MkdirTemp("", "sshore-cli-test-")
		if cliBuild.err != nil {
			return
		}
		cliBuild.binary = filepath.Join(cliBuild.directory, "sshore")
		cmd := exec.Command("go", "build", "-tags=sshoretest", "-buildvcs=false", "-race", "-o", cliBuild.binary, "..")
		if output, err := cmd.CombinedOutput(); err != nil {
			cliBuild.err = fmt.Errorf("build CLI: %w\n%s", err, output)
		}
	})
	if cliBuild.err != nil {
		t.Fatal(cliBuild.err)
	}
	return cliBuild.binary
}

type cliFixture struct {
	bin, dir string
	env      []string
}

// These fields are the documented JSON command output, not persisted records.
type cliShell struct {
	ID    uint64 `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	PID   int    `json:"pid"`
}

func newCLI(t *testing.T) *cliFixture {
	t.Helper()
	e := &cliFixture{bin: testBinary(t), dir: t.TempDir()}
	e.env = append(os.Environ(), "HOME="+e.dir, "SSHORE_STATE_DIR="+filepath.Join(e.dir, "state"), "SSHORE_RUNTIME_DIR="+filepath.Join(e.dir, "runtime"), "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=", "SSHORE_DISABLED=", "SSH_CONNECTION=", "SSHORE_TREE_ID=", "GORACE=atexit_sleep_ms=0", "SHELL=/bin/sh", "PS1=sshore-test> ")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, e.bin, "cancel", "--all")
		cmd.Env, cmd.Stdin = e.env, strings.NewReader("cancel\n")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("CLI cleanup: %v\n%s", err, output)
		}
	})
	return e
}

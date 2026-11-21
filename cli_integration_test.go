package main

import (
	"context"
	"encoding/json"
	"github.com/rogpeppe/go-internal/testscript"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cliFixture struct {
	bin, dir string
	env      []string
}

func fixtureDaemon(t *testing.T, state string) localShellRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "daemon", "daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record localShellRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// These fields are the documented JSON command output, not persisted records.
type cliShell struct {
	ShortID string `json:"short_id"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"`
	PID     int    `json:"pid"`
}

func (e *cliFixture) shells(t *testing.T) []cliShell {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "list", "here:", "--json")
	cmd.Env = e.env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var shells []cliShell
	if err := json.Unmarshal(out, &shells); err != nil {
		t.Fatalf("list output: %v: %s", err, out)
	}
	return shells
}

func newCLI(t *testing.T) *cliFixture {
	t.Helper()
	e := &cliFixture{bin: testBinary(t), dir: t.TempDir()}
	e.env = append(os.Environ(), "HOME="+e.dir, "SSHORE_STATE_DIR="+filepath.Join(e.dir, "state"), "SSHORE_RUNTIME_DIR="+filepath.Join(e.dir, "runtime"), "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=", "SSHORE_DISABLED=", "SSH_CONNECTION=", "SSHORE_TREE_ID=", "GORACE=atexit_sleep_ms=0", "SHELL=/bin/sh", "PS1=sshore-test> ")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, e.bin, "kill", "--all")
		cmd.Env, cmd.Stdin = e.env, strings.NewReader("kill\n")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("CLI cleanup: %v\n%s", err, output)
		}
	})
	return e
}

func TestScripts(t *testing.T) {
	bin := testBinary(t)
	repo, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	testscript.Run(t, testscript.Params{
		Dir: "testdata/cli",
		Setup: func(env *testscript.Env) error {
			env.Setenv("REPO", repo)
			env.Setenv("SSHORE_BIN", bin)
			env.Setenv("SSHORE_TEST_CLI", "1")
			return nil
		},
	})
}

//go:build sshoretest

package tests_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		if runtime.GOOS == "windows" {
			cliBuild.binary += ".exe"
		}
		cmd := exec.Command("go", "build", "-tags=sshoretest", "-buildvcs=false", "-o", cliBuild.binary, "..")
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
	cmd := exec.CommandContext(ctx, e.bin, "list", "--local", "--json")
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

func TestHelpAndExamplesBeforeExecution(t *testing.T) {
	bin := testBinary(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "unused-state")
	env := append(os.Environ(), "SSHORE_DISABLED=1", "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=")
	type helpCase struct {
		args  []string
		valid bool
	}
	cases := []helpCase{
		{[]string{"--tldr"}, true},
		{[]string{"completion", "bash"}, false},
		{[]string{"__complete", "words"}, false},
		{[]string{"--tldr", "cp"}, false},
		{[]string{"detach", "--tldr"}, false},
		{[]string{"shell", "--help"}, false},
	}
	for _, command := range []string{"launch", "spawn", "attach", "detach", "exec", "list", "cp", "mount", "forward", "review", "kill", "rename"} {
		cases = append(cases, helpCase{[]string{command, "--help"}, true})
	}
	for _, command := range []string{"launch", "spawn", "cp", "mnt", "fwd"} {
		cases = append(cases, helpCase{[]string{command, "--tldr"}, true})
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, tc.args...)
			cmd.Env, cmd.Dir = env, dir
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil || (err == nil) != tc.valid {
				t.Fatalf("%v: %v: %s", tc.args, err, out)
			}
		})
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("help changed application state: %v", err)
	}
}

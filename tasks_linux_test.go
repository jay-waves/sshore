package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chzyer/readline"
	"github.com/jay-waves/sshore/tests/testutil/sshtest"
	cryptossh "golang.org/x/crypto/ssh"
)

// Uses the existing isolated CLI test executable and isolated daemon fixture. The
// SSH wrapper runs only fixture commands; no real account sessions or hosts.

func TestKillAllRemoteScope(t *testing.T) {
	e := newEnv(t)
	first, firstPID := e.attach("one")
	second, secondPID := e.attach("two")
	defer first.close()
	defer second.close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wrapper := sshtest.Start(t, sshtest.Options{Exec: func(ctx context.Context, command string, stream cryptossh.Channel) {
		sshtest.Shell(ctx, command, stream, e.dir, e.env)
	}})
	env := append(e.env, "SSHORE_SSH="+wrapper, "SSHORE_SOCKET=", "SSHORE_STATE_DIR="+filepath.Join(e.dir, "local-state"))
	run := func(input string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("", "fixture-launch", "here:", "keep", "--detach"); err != nil {
		t.Fatal(out, err)
	}
	defer run("", "kill", "here:keep")
	if out, err := run("kill\n", "kill", "fixture", "--all", "--server", e.bin); err != nil {
		t.Fatal(out, err)
	}
	waitSessionEnded(t, e, "one", firstPID)
	waitSessionEnded(t, e, "two", secondPID)
	out, err := run("", "list", "here:", "--json")
	var rows []localShellView
	if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].Name != "keep" {
		t.Fatal(out, err)
	}
}

func TestListScopes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	tasks := startRequestDaemon(t)
	registerTestResource(t, tasks, "forward", "local-only", strings.Repeat("a", 32))
	other, _ := registerTestResource(t, tasks, "forward", "other", strings.Repeat("b", 32))
	other.record.Host = "elsewhere"
	run := func(args ...string) (inventory, string, error) {
		cmd := exec.Command(testBinary(t), args...)
		cmd.Env = append(os.Environ(), "SSHORE_SSH="+filepath.Join(root, "must-not-run"), "SSHORE_SOCKET=", "SSH_CONNECTION=", "HOME="+root)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		inv := emptyInventory()
		if err == nil {
			err = json.Unmarshal(out, &inv)
		}
		return inv, stderr.String(), err
	}
	local, _, err := run("list", "--json")
	if err != nil || len(local.Resources) != 2 {
		t.Fatalf("%+v %v", local, err)
	}
	remote, stderr, err := run("list", "work", "--json")
	if err != nil || stderr != "" || len(remote.Resources) != 1 || remote.Resources[0].ID != "forward/local-only" || len(remote.Shells) != 0 {
		t.Fatalf("%+v %s %v", remote, stderr, err)
	}
	empty, stderr, err := run("list", "unknown:", "--json")
	if err != nil || stderr != "" || len(empty.Resources) != 0 || len(empty.Shells) != 0 {
		t.Fatalf("%+v %s %v", empty, stderr, err)
	}
}

func TestExitReceiptInventoryAndReset(t *testing.T) {
	activityFixture(t)
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	dir, err := daemonDirectory()
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacyShellRuntime()
	if err := os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, fmt.Sprintf("%032x.exit.json", 1)), filepath.Join(legacy, fmt.Sprintf("%032x.sock.exit.json", 2))} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// An older daemon can report a separate runtime directory inherited at startup.
	inherited := filepath.Join(root, "inherited")
	if err := os.Mkdir(inherited, 0700); err != nil {
		t.Fatal(err)
	}
	oldSocket := filepath.Join(inherited, fmt.Sprintf("%032x.sock", 4))
	for _, path := range []string{oldSocket, oldSocket + ".exit.json", filepath.Join(legacy, "unrelated.exit.json")} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := resetHostedExitState(dir, []string{inherited}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{oldSocket, oldSocket + ".exit.json", filepath.Join(dir, fmt.Sprintf("%032x.exit.json", 1))} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("reset retained %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(legacy, "unrelated.exit.json")); err != nil {
		t.Fatal("reset removed unrelated file", err)
	}
}

func TestTaskInventoryOutputTerminalWidth(t *testing.T) {
	for _, columns := range []int{60, 100} {
		t.Run(strconv.Itoa(columns), func(t *testing.T) {
			var output bytes.Buffer
			inv := inventory{Resources: []resourceView{{ID: "mount/data", Host: "server", Worker: "running", Detail: strings.Repeat("路径/", 40)}}}
			writeTaskInventoryWidth(&output, inv, "", false, columns)
			for line := range strings.SplitSeq(output.String(), "\n") {
				if (readline.Runes{}).WidthAll([]rune(line)) > columns {
					t.Fatalf("width %d: %q", columns, line)
				}
			}
			if !strings.Contains(output.String(), "…") {
				t.Fatal(output.String())
			}
		})
	}
}

func TestListIncludesAccountHostedShells(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("hosted")
	defer b.close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "list", "--json")
	cmd.Env = e.env
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("list: %v: %s", err, output)
	}
	var inv inventory
	if err := json.Unmarshal(output, &inv); err != nil {
		t.Fatal(err)
	}
	for _, s := range inv.Shells {
		if s.Name == "hosted" && s.Host == "local" && s.State == "attached" && s.ShortID != "" {
			return
		}
	}
	t.Fatalf("hosted shell missing from list: %s", output)
}

func TestKillAllIncludesAccountHostedShells(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("hosted")
	defer b.close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "kill", "--all")
	cmd.Env = e.env
	cmd.Stdin = strings.NewReader("kill\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cancel hosted shell: %s %v", out, err)
	}
	waitSessionEnded(t, e, "hosted", pid)
}

func TestHostedShellTaskCommandRouting(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	e := newEnv(t)
	t.Setenv("SSHORE_RUNTIME_DIR", e.dir)
	b, _ := e.attach("hosted")
	defer b.close()
	inv, err := collectInventory(options{})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, s := range inv.Shells {
		if s.Name == "hosted" {
			id = s.ShortID
		}
	}
	if id == "" {
		t.Fatal("hosted shell not listed")
	}
	detachTestClient(t, b)
	automatic := options{command: "attach", local: true}
	if err := resolveDetachedAttach(&automatic); err != nil || !automatic.hosted || automatic.instance == "" {
		t.Fatalf("automatic local attachment lost hosted route: %+v %v", automatic, err)
	}
	choices := localSessionTargets("other", message{Action: "select", Name: "hosted"})
	if choices.Error != "" || len(choices.Targets) != 1 || choices.Targets[0].Name != "hosted" || !choices.Targets[0].Hosted {
		t.Fatalf("hosted shell missing from local switcher: %+v", choices)
	}
	for _, args := range [][]string{{id}, {"here:hosted"}} {
		parsed, err := parseCLI(args, "attach")
		if err != nil {
			t.Fatal(err)
		}
		if task, ok, err := resolveShortTask("attach", &parsed); err != nil || !ok || !task.Hosted {
			t.Fatalf("hosted selector %v: %+v %v", args, task, err)
		}
	}
	parsed, err := parseCLI([]string{id}, "attach")
	if err != nil {
		t.Fatal(err)
	}
	target, selected, err := resolveShortTask("attach", &parsed)
	if err != nil || !selected || !target.Hosted {
		t.Fatalf("wrong local route: %+v %v", target, err)
	}
	o := options{command: "attach", local: true, hosted: true, session: target.Name, instance: target.Instance}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := openClientTransportContext(ctx, &o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientAttachment(c.(attachmentControl)); err != nil {
		c.Close()
		t.Fatal(err)
	}
	c.Close()
	if code, err := run([]string{"rename", "here:" + id, "renamed"}); code != 0 || err != nil {
		t.Fatalf("rename listed ID: %d %v", code, err)
	}
	if code, err := run([]string{"kill", "here:" + id}); code != 0 || err != nil {
		t.Fatalf("cancel renamed ID: %d %v", code, err)
	}
}

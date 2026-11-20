package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chzyer/readline"
	"github.com/creack/pty"
	"github.com/jay-waves/sshore/tests/testutil/sshtest"
	cryptossh "golang.org/x/crypto/ssh"
)

// Uses the existing isolated CLI test executable and isolated daemon fixture. The
// SSH wrapper runs only fixture commands; no real account sessions or hosts.

func TestApprovedForwardIndependent(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	e := newEnv(t)
	remoteHome := t.TempDir()
	e.env = append(e.env, "HOME="+remoteHome)
	b, pid := e.attach("main")
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	sendTest(t, b.in, message{Type: "bye"})
	b.close()
	var reviewCommands atomic.Int32
	wrapper := sshtest.Start(t, sshtest.Options{Exec: func(ctx context.Context, command string, stream cryptossh.Channel) {
		reviewCommands.Add(1)
		sshtest.Shell(ctx, command, stream, e.dir, e.env)
	}})
	env := append(e.env, "SSHORE_SSH="+wrapper)
	instance, err := resolveSession("main")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := queueRequest(options{host: "fixture", session: "main", server: e.bin, shellPID: pid, instance: instance}, []operation{{Kind: "forward", TargetPort: 31235}})
	if err != nil {
		t.Fatal(err)
	}
	id := queued.ID
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = env
		var stderr strings.Builder
		cmd.Stderr = &stderr
		b, err := cmd.Output()
		if err != nil {
			b = append(b, []byte(stderr.String())...)
		}
		return string(b), err
	}
	publishPending := func(id string) {
		t.Helper()
		if err := storeRemoteTask(pendingRemoteTask(id, reviewName(t, id), []operation{{Kind: "forward", TargetPort: 31235}})); err != nil {
			t.Fatal(err)
		}
	}
	publishPending(id)
	out, err := run("list")
	if err != nil || !strings.Contains(out, reviewName(t, id)) || !strings.Contains(out, "review") || !strings.Contains(out, "pending") {
		t.Fatal(out, err)
	}
	name := reviewName(t, id)
	defer run("kill", "forward/"+name)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "review", "review/"+reviewName(t, id))
	cmd.Env = env
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 160})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer cmd.Process.Kill()
	output := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		answered := false
		for {
			n, e := terminal.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
				if !answered && strings.Contains(b.String(), "Local listening port") {
					io.WriteString(terminal, "31234\r")
					answered = true
				}
			}
			if e != nil {
				break
			}
		}
		output <- b.String()
	}()
	if err = cmd.Wait(); err != nil {
		terminal.Close()
		t.Fatalf("approve: %v %s", err, <-output)
	}
	out = <-output
	if got := reviewCommands.Load(); got != 1 {
		t.Fatalf("review opened %d command sessions, want 1", got)
	}
	if !strings.Contains(out, "Started forward/"+name) {
		t.Fatal(out)
	}
	resumed, resumedPID := e.attach("main")
	if resumedPID != pid {
		t.Fatal("source shell was replaced")
	}
	resumed.writeInput(t, []byte("exit\n"))
	resumed.readExit(t)
	resumed.close()
	out, err = run("list")
	if err != nil || !strings.Contains(out, name) || !strings.Contains(out, "forward") || !strings.Contains(out, "running") || strings.Contains(out, " review ") {
		t.Fatal(out, err)
	}

	out, err = run("list", "--json")
	var snapshot inventory
	if err != nil || json.Unmarshal([]byte(out), &snapshot) != nil || len(snapshot.Resources) != 1 || snapshot.Resources[0].Target != "unchecked" {
		t.Fatal(out, err)
	}
	out, err = run("list", "fixture", "--json", "--server", e.bin)
	if err != nil || json.Unmarshal([]byte(out), &snapshot) != nil || len(snapshot.Exits) == 0 {
		t.Fatal(out, err)
	}
	// The independent watcher synchronizes only this resource to the peer list.
	// This fixture uses one account on both ends, so list hides the duplicate.
	deadline := time.Now().Add(3 * time.Second)
	for {
		peers, err := collectPeerViews()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, peer := range peers {
			found = found || strings.HasPrefix(peer.ID, "peer/") && strings.HasSuffix(peer.ID, "/forward/"+name)
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("missing peer task", peers)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, err = run("kill", name); err != nil {
		t.Fatal(out, err)
	}
	out, err = run("list")
	if err != nil || strings.Contains(out, "forward/"+name) {
		t.Fatal(out, err)
	}
}

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
	state := filepath.Join(root, "state")
	t.Setenv("SSHORE_STATE_DIR", state)
	tasks := startRequestDaemon(t)
	registerTestResource(t, tasks, "forward", "local-only", strings.Repeat("a", 32))
	var err error
	if err = saveShellSnapshot(shellSnapshot{Host: "work", Name: "cached", State: "detached", ConfirmedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var called, fail atomic.Bool
	configTool := sshtest.Start(t, sshtest.Options{Exec: func(_ context.Context, _ string, stream cryptossh.Channel) {
		called.Store(true)
		status := uint32(0)
		if fail.Load() {
			status = 23
		} else {
			_, _ = io.WriteString(stream, `{"version":"`+version+`","resources":[],"shells":[{"name":"remote-only","state":"detached"}],"peers":[],"errors":[]}`+"\n")
		}
		sshtest.Exit(stream, status)
	}})
	run := func(args ...string) (inventory, string, error) {
		cmd := exec.Command(testBinary(t), args...)
		cmd.Env = append(os.Environ(), "SSHORE_SSH="+configTool, "SSHORE_SOCKET=", "SSH_CONNECTION=", "SSHORE_RUNTIME_DIR="+filepath.Join(root, "runtime"), "HOME="+root)
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
	if err != nil || local.RemoteQueried || len(local.Resources) != 1 || len(local.Shells) != 1 {
		t.Fatalf("%+v %v", local, err)
	}
	if called.Load() {
		t.Fatal("offline list invoked SSH")
	}
	remote, stderr, err := run("list", "work", "--json")
	if err != nil || !remote.RemoteQueried || len(remote.Resources) != 0 || len(remote.Shells) != 1 || remote.Shells[0].Name != "remote-only" || stderr != "" {
		t.Fatalf("%+v %s %v", remote, stderr, err)
	}
	fail.Store(true)
	_, stderr, err = run("list", "work", "--json")
	if err == nil || !strings.Contains(stderr, "remote query failed") {
		t.Fatalf("%s %v", stderr, err)
	}
}

func TestInventoryPreservesOtherModulesAfterShellFailure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	brokenRuntime := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(brokenRuntime, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_RUNTIME_DIR", brokenRuntime)
	startRequestDaemon(t)
	if _, err := queueRequest(testRequestOptions(), []operation{{Kind: "upload", Path: "/tmp"}}); err != nil {
		t.Fatal(err)
	}
	inv := localSessionInventory()
	if len(inv.Errors) == 0 || len(inv.Resources) != 1 || inv.Resources[0].Kind != "review" {
		t.Fatalf("shell failure discarded other task observations: %+v", inv)
	}
}

func TestInventoryOmitsExpiredExitReceipts(t *testing.T) {
	activityFixture(t)
	root := t.TempDir()
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
	dir, err := runtimeDir()
	if err != nil {
		t.Fatal(err)
	}
	for name, age := range map[string]time.Duration{"recent": time.Hour, "expired": 8 * 24 * time.Hour} {
		if err := writeStateJSON(filepath.Join(dir, name+".sock.exit.json"), sessionExit{Name: name, Reason: "exited", At: time.Now().Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	inv := localSessionInventory()
	if len(inv.Errors) != 0 {
		t.Fatal(inv.Errors)
	}
	if len(inv.Exits) != 1 || inv.Exits[0].Name != "recent" {
		t.Fatalf("exit receipts: %#v", inv.Exits)
	}
}

func TestKillUpdatesShellHistory(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	for _, exit := range []uint32{23, 0} {
		config := sshtest.Start(t, sshtest.Options{Exec: func(_ context.Context, _ string, stream cryptossh.Channel) {
			sshtest.Exit(stream, exit)
		}})
		t.Setenv("SSHORE_SSH", config)
		if err := saveShellSnapshot(shellSnapshot{Host: "work", Name: "job", Instance: "instance", State: "detached", ConfirmedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		code, err := runClientOnce(options{host: "work", session: "job", instance: "instance", command: "kill", server: "sshore"}, "", nil, func(options) {})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := readShellSnapshots()
		if err != nil || len(rows) != 1 {
			t.Fatalf("%+v %v", rows, err)
		}
		if exit == 0 {
			if code != 0 || rows[0].State != "exited" {
				t.Fatalf("%d %+v", code, rows)
			}
		} else if code != 23 || rows[0].State != "detached" {
			t.Fatalf("failure changed history: %d %+v", code, rows)
		}
	}
}

func TestShortTaskRemoteCommandRouting(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	root := t.TempDir()
	observation := filepath.Join(root, "ssh-arguments")
	config := filepath.Join(root, "ssh-config")
	script := "#!/bin/sh\nprintf '%s\\0' \"$PWD\" \"$@\" > " + shellQuote(observation) + "\nprintf 'routing probe stopped\\n' >&2\nexit 37\n"
	if err := os.WriteFile(config, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_SSH", config)
	observed := options{host: "work", server: "/observed/sshore", sshDirectory: root, ssh: []string{"-p", "2222", "-J", "jump", "-L", "8080:localhost:80"}}
	inv := inventory{Shells: []shellView{{Name: "job", Instance: "original-instance", State: "detached"}}}
	if err := cacheRemoteShells("work", inv, observed); err != nil {
		t.Fatal(err)
	}
	// A later attachment default must not overwrite the selected observation's route.
	if err := rememberConnection(options{host: "work", session: "other", server: "/other/sshore", ssh: []string{"-p", "9999"}}); err != nil {
		t.Fatal(err)
	}
	id := taskShortCode(remoteShellIdentity("work", "original-instance"))
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args                      []string
		dir, port, server, action string
	}{
		{[]string{"kill", "work:" + id}, root, "2222", "/observed/sshore", "kill-session"},
		{[]string{"rename", "work:" + id, "new"}, root, "2222", "/observed/sshore", "rename-session"},
		{[]string{"kill", "work:" + id, "--server", "/explicit/sshore", "--", "-p", "3333"}, workingDir, "3333", "/explicit/sshore", "kill-session"},
	} {
		if _, err := run(tc.args); err == nil || !strings.Contains(err.Error(), "routing probe stopped") {
			t.Fatalf("%v: %v", tc.args, err)
		}
		raw, err := os.ReadFile(observation)
		if err != nil {
			t.Fatal(err)
		}
		configArgs := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		if len(configArgs) < 5 || configArgs[0] != tc.dir || configArgs[1] != "-G" || configArgs[len(configArgs)-2] != "work" {
			t.Fatalf("wrong SSH config invocation: %q", configArgs)
		}
		text := configArgs[len(configArgs)-1]
		flags := configArgs[2 : len(configArgs)-3]
		foundPort := false
		for i := 0; i+1 < len(flags); i++ {
			if flags[i] == "-p" {
				if foundPort || flags[i+1] != tc.port {
					t.Fatalf("wrong SSH port in %q", flags)
				}
				foundPort = true
			}
		}
		if !foundPort || slices.Contains(flags, "-L") || slices.Contains(flags, "8080:localhost:80") {
			t.Fatalf("wrong transport restored: %q", flags)
		}
		for _, want := range []string{tc.server, tc.action, "original-instance"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q in %s", want, text)
			}
		}
	}
	records, err := readRecentConnections()
	if err != nil || len(records) != 1 || records[0].Session != "other" {
		t.Fatalf("attachment default changed: %+v %v", records, err)
	}
	public, err := collectInventory(options{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "/observed/sshore") || strings.Contains(string(encoded), "2222") {
		t.Fatal("private connection settings leaked into inventory")
	}
}

func TestTaskInventoryOutputTerminalWidth(t *testing.T) {
	for _, columns := range []uint16{60, 100} {
		t.Run(strconv.Itoa(int(columns)), func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			if err := pty.Setsize(slave, &pty.Winsize{Rows: 24, Cols: columns}); err != nil {
				t.Fatal(err)
			}
			writeTaskInventory(slave, inventory{Resources: []resourceView{{ID: "mount/data", Host: "server", Worker: "running", Detail: strings.Repeat("路径/", 40)}}})
			reader := bufio.NewReader(master)
			var output strings.Builder
			for range 3 {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				line = strings.TrimRight(line, "\r\n")
				if (readline.Runes{}).WidthAll([]rune(line)) > int(columns) {
					t.Fatalf("width %d: %q", columns, line)
				}
				output.WriteString(line)
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
		if s.Name == "hosted" && s.Host == "local" && s.State == "attached" && !s.Historical && s.ShortID != "" {
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
	if err := resolveLocalAttach(&automatic); err != nil || !automatic.hosted || automatic.instance == "" {
		t.Fatalf("automatic local attachment lost hosted route: %+v %v", automatic, err)
	}
	choices := localSessionTargets("other", message{Action: "select", Name: "hosted"})
	if choices.Error != "" || len(choices.Targets) != 1 || choices.Targets[0].Name != "hosted" || !choices.Targets[0].Hosted {
		t.Fatalf("hosted shell missing from local switcher: %+v", choices)
	}
	for _, args := range [][]string{{id}, {"hosted"}} {
		parsed, err := parseCLI(args, "attach")
		if err != nil {
			t.Fatal(err)
		}
		if task, ok, err := resolveShortTask("attach", &parsed); err != nil || !ok || !task.hosted {
			t.Fatalf("hosted selector %v: %+v %v", args, task, err)
		}
	}
	parsed, err := parseCLI([]string{id}, "attach")
	if err != nil {
		t.Fatal(err)
	}
	target, selected, err := resolveShortTask("attach", &parsed)
	if err != nil || !selected || !target.hosted || target.host != "" {
		t.Fatalf("wrong local route: %+v %v", target, err)
	}
	o := options{command: "attach", local: true, hosted: true, session: target.name, instance: target.instance}
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

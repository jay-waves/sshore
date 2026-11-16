package main

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/jay-waves/sshore/internal/sshtest"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chzyer/readline"
	"github.com/creack/pty"
)

// Uses the existing sshoretest-tagged binary and isolated daemon fixture. The
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
	wrapper := filepath.Join(e.dir, "ssh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf x >> "+shellQuote(filepath.Join(e.dir, "ssh-connections"))+"\nfor last; do :; done\nexec /bin/sh -c \"$last\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	wrapper = sshtest.Wrap(t, wrapper, e.env)
	env := append(e.env, "SSHORE_SSH="+wrapper)
	id, err := writeApproval(options{host: "fixture", session: "main", server: e.bin, resumePID: pid}, []operation{{Kind: "forward", TargetPort: 31235}})
	if err != nil {
		t.Fatal(err)
	}
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
		dir := filepath.Join(remoteHome, filepath.FromSlash(remoteTaskRelativeDir))
		if e := os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
		b, e := json.Marshal(pendingRemoteTask(id, reviewName(t, id), []operation{{Kind: "forward", TargetPort: 31235}}))
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dir, id+".json"), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	publishPending(id)
	out, err := run("list")
	if err != nil || !strings.Contains(out, reviewName(t, id)) || !strings.Contains(out, "review") || !strings.Contains(out, "pending") {
		t.Fatal(out, err)
	}
	name := reviewName(t, id)
	defer run("cancel", "forward/"+name)
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
	connections, readErr := os.ReadFile(filepath.Join(e.dir, "ssh-connections"))
	if readErr != nil || string(connections) != "x" {
		t.Fatalf("review must authenticate only once: %q %v", connections, readErr)
	}
	if !strings.Contains(out, "Started forward/"+name) {
		t.Fatal(out)
	}
	resumed, resumedPID := e.attach("main")
	if resumedPID != pid {
		t.Fatal("source shell was replaced")
	}
	sendTest(t, resumed.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, resumed.tty, "exit")
	resumed.close()
	out, err = run("list")
	if err != nil || !strings.Contains(out, name) || !strings.Contains(out, "forward") || !strings.Contains(out, "running") || strings.Contains(out, " review ") {
		t.Fatal(out, err)
	}
	if _, e := os.Stat(filepath.Join(remoteHome, filepath.FromSlash(remoteTaskRelativeDir), id+".json")); !os.IsNotExist(e) {
		t.Fatal("approved task mirror survived", e)
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
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, err = run("list", "--json")
		if err != nil || json.Unmarshal([]byte(out), &snapshot) != nil {
			t.Fatal(out, err)
		}
		found := false
		for _, peer := range snapshot.Peers {
			found = found || strings.HasPrefix(peer.ID, "peer/") && strings.HasSuffix(peer.ID, "/forward/"+name)
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("missing peer task", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Direct forwards no longer accept an explicit resource name.
	if out, err = run("forward", "origin:31234", "fixture:31235", "--name", name); err == nil || !strings.Contains(out, "unsupported option: --name") {
		t.Fatal("expected unsupported name option", out, err)
	}
	renamed := "renamed-forward"
	if out, err = run("rename", "forward/"+name, renamed); err != nil {
		t.Fatal(out, err)
	}
	out, err = run("list", "--json")
	if err != nil || json.Unmarshal([]byte(out), &snapshot) != nil || len(snapshot.Resources) != 1 ||
		snapshot.Resources[0].ID != "forward/"+renamed || snapshot.Resources[0].Worker != "running" {
		t.Fatal(out, err)
	}
	if snapshot.Resources[0].ShortID != name {
		t.Fatal("alias changed the task ID", out)
	}
	if out, err = run("rename", name, "renamed-again"); err != nil {
		t.Fatal(out, err)
	}
	if out, err = run("cancel", "forward/"+renamed); err == nil {
		t.Fatal("old alias still selects the forward", out)
	}
	if out, err = run("cancel", name); err != nil {
		t.Fatal(out, err)
	}
	out, err = run("list")
	if err != nil || strings.Contains(out, "forward/"+name) {
		t.Fatal(out, err)
	}
	rejected, e2 := writeApproval(options{host: "fixture", session: "main", server: e.bin, resumePID: pid}, []operation{{Kind: "forward", TargetPort: 31235}})
	if e2 != nil {
		t.Fatal(e2)
	}
	publishPending(rejected)
	dir, e2 := approvalDir()
	if e2 != nil {
		t.Fatal(e2)
	}
	request, e2 := readApproval(dir, rejected)
	if e2 != nil {
		t.Fatal(e2)
	}
	if out, err = run("rename", "review/"+request.DisplayName(), "renamed-request"); err != nil {
		t.Fatal(out, err)
	}
	if request, e2 := findApproval(dir, "renamed-request"); e2 != nil || request.ID != rejected {
		t.Fatal("rename changed the approval identity", request, e2)
	}
	if out, err = run("cancel", "review/renamed-request"); err != nil {
		t.Fatal(out, err)
	}
	if _, e2 = os.Stat(filepath.Join(remoteHome, filepath.FromSlash(remoteTaskRelativeDir), rejected+".json")); !os.IsNotExist(e2) {
		t.Fatal("rejected mirror survived", e2)
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
	wrapper = sshtest.Wrap(t, wrapper, e.env)
	env := append(e.env, "SSHORE_SSH="+wrapper, "SSHORE_SOCKET=", "SSHORE_STATE_DIR="+filepath.Join(e.dir, "local-state"))
	run := func(input string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("", "launch", "--local", "keep", "--detach"); err != nil {
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

func TestListScopes(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("SSHORE_STATE_DIR", state)
	dir, err := resourceStateDir("forward")
	if err != nil {
		t.Fatal(err)
	}
	if err = writeStateJSON(filepath.Join(dir, "local-only.json"), resourceRecord{Kind: "forward", Name: "local-only", Host: "work", Version: version}); err != nil {
		t.Fatal(err)
	}
	if err = saveShellSnapshot(shellSnapshot{Host: "work", Name: "cached", State: "detached", ConfirmedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "ssh-called")
	wrapper := filepath.Join(root, "ssh")
	if err = os.WriteFile(wrapper, []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\nprintf '%s\\n' "+shellQuote(`{"version":"`+version+`","resources":[],"shells":[{"name":"remote-only","state":"detached"}],"peers":[],"errors":[]}`)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	configTool := sshtest.Wrap(t, wrapper, os.Environ())
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
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("offline list invoked SSH")
	}
	remote, stderr, err := run("list", "work", "--json")
	if err != nil || !remote.RemoteQueried || len(remote.Resources) != 0 || len(remote.Shells) != 1 || remote.Shells[0].Name != "remote-only" || stderr != "" {
		t.Fatalf("%+v %s %v", remote, stderr, err)
	}
	if err = os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = run("list", "work", "--json")
	if err == nil || !strings.Contains(stderr, "remote query failed") {
		t.Fatalf("%s %v", stderr, err)
	}
}

func TestInventoryOmitsExpiredExitReceipts(t *testing.T) {
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
	inv, err := localSessionInventory()
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Exits) != 1 || inv.Exits[0].Name != "recent" {
		t.Fatalf("exit receipts: %#v", inv.Exits)
	}
}

func TestCancelUpdatesShellHistory(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	wrapper := filepath.Join(t.TempDir(), "ssh")
	t.Setenv("SSHORE_SSH", wrapper)
	for _, exit := range []string{"23", "0"} {
		if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit "+exit+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SSHORE_SSH", sshtest.Wrap(t, wrapper, os.Environ()))
		if err := saveShellSnapshot(shellSnapshot{Host: "work", Name: "job", Instance: "instance", State: "detached", ConfirmedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		code, err := runClientOnce(options{host: "work", session: "job", instance: "instance", command: "cancel", server: "sshore"}, "", func() {})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := readShellSnapshots()
		if err != nil || len(rows) != 1 {
			t.Fatalf("%+v %v", rows, err)
		}
		if exit == "0" {
			if code != 0 || rows[0].State != "exited" {
				t.Fatalf("%d %+v", code, rows)
			}
		} else if code != 23 || rows[0].State != "detached" {
			t.Fatalf("failure changed history: %d %+v", code, rows)
		}
	}
}

func TestShortTaskRemoteCommandRouting(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	root := t.TempDir()
	wrapper, output := filepath.Join(root, "ssh"), filepath.Join(root, "args")
	t.Setenv("SSHORE_SSH", wrapper)
	t.Setenv("SSHORE_TEST_ARGS", output)
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SSHORE_TEST_ARGS\"\nexit 37\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_SSH", sshtest.Wrap(t, wrapper, os.Environ()))
	observed := options{host: "work", server: "/observed/sshore", sshDirectory: root, ssh: []string{"-p", "2222", "-J", "jump", "-L", "8080:localhost:80"}}
	inv := inventory{Shells: []shellView{{Name: "job", Instance: "original-instance", State: "detached"}}}
	if err := cacheRemoteShells("work", inv, observed); err != nil {
		t.Fatal(err)
	}
	// A later attachment default must not overwrite the selected observation's route.
	if err := rememberConnection(options{host: "work", session: "other", server: "/other/sshore", ssh: []string{"-p", "9999"}}); err != nil {
		t.Fatal(err)
	}
	ids, err := assignTaskIDs([]string{remoteShellIdentity("work", "original-instance")})
	if err != nil {
		t.Fatal(err)
	}
	id := ids[remoteShellIdentity("work", "original-instance")].code
	for _, tc := range []struct {
		args                 []string
		port, server, action string
	}{
		{[]string{"cancel", id}, "2222", "/observed/sshore", "kill-session"},
		{[]string{"rename", id, "new"}, "2222", "/observed/sshore", "rename-session"},
		{[]string{"cancel", id, "--server", "/explicit/sshore", "--", "-p", "3333"}, "3333", "/explicit/sshore", "kill-session"},
	} {
		if code, err := run(tc.args); err != nil || code != 37 {
			t.Fatalf("%v: %d %v", tc.args, code, err)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, want := range []string{tc.server, tc.action, "original-instance"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q in %s", want, text)
			}
		}
		if strings.Contains(text, "9999") || strings.Contains(text, "8080") {
			t.Fatal("wrong transport restored", text)
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

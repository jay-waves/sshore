package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/jay-waves/sshore/internal/sshtest"
	"github.com/jay-waves/sshore/internal/wire"
	"github.com/xtaci/smux"
)

func TestLocalShellDetachAndReattach(t *testing.T) {
	e := newEnv(t)
	state := filepath.Join(e.dir, "local-state")
	t.Cleanup(func() { shutdownFixtureDaemon(t, state) })
	env := append(e.env, "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSH_CONNECTION=", "SHELL=/bin/sh", "TERM=xterm-256color")
	for _, args := range [][]string{{"fixture-launch", "--local", "localtest", "--no-profile"}, {"attach", "self:"}} {
		if args[0] == "attach" {
			list := exec.Command(e.bin, "list", "--local", "--json")
			list.Env = env
			data, err := list.Output()
			if err != nil {
				t.Fatal(err)
			}
			var rows []localShellView
			if err = json.Unmarshal(data, &rows); err != nil || len(rows) != 1 {
				t.Fatalf("list: %s %v", data, err)
			}
			originalID := rows[0].ShortID
			rename := exec.Command(e.bin, "rename", "self:"+originalID, "renamed")
			rename.Env = env
			if out, err := rename.CombinedOutput(); err != nil {
				t.Fatalf("rename: %s %v", out, err)
			}
			list = exec.Command(e.bin, "list", "--local", "--json")
			list.Env = env
			data, err = list.Output()
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(data, &rows); err != nil || len(rows) != 1 || rows[0].Name != "renamed" || rows[0].ShortID != originalID {
				t.Fatalf("renamed list: %s %v", data, err)
			}
			args = []string{"attach", "self:" + originalID}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = env
		terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 100})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		output := make(chan string, 1)
		go func() {
			var out strings.Builder
			buf := make([]byte, 4096)
			stage := 0
			for {
				n, err := terminal.Read(buf)
				out.Write(buf[:n])
				if stage == 0 && strings.Contains(out.String(), "Connected self:") {
					stage = 1
					if args[0] == "fixture-launch" {
						io.WriteString(terminal, "\x02c")
					} else {
						io.WriteString(terminal, "exit\n")
					}
				}
				if args[0] == "fixture-launch" && stage == 1 && strings.Count(out.String(), "Connected self:") >= 2 {
					stage = 2
					io.WriteString(terminal, "\x02d")
				}
				if args[0] == "fixture-launch" && stage == 2 && strings.Count(out.String(), "Connected self:localtest") >= 2 {
					stage = 3
					io.WriteString(terminal, "\x02d")
				}
				if err != nil {
					break
				}
			}
			output <- out.String()
		}()
		err = cmd.Wait()
		terminal.Close()
		cancel()
		out := <-output
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		if args[0] == "fixture-launch" {
			t.Setenv("SSHORE_STATE_DIR", state)
			rows, err := nativeShells()
			if err != nil {
				t.Fatal(err)
			}
			var first, second localShellRecord
			for _, row := range rows {
				conn, record, err := openNativeShell(context.Background(), "", row.identity)
				if err != nil {
					t.Fatal(err)
				}
				conn.Close()
				if record.Name == "localtest" {
					first = record
				} else {
					second = record
				}
			}
			if !validTaskUID(first.UID) || !validTaskUID(second.UID) || second.Name != second.UID[:6] {
				t.Fatalf("invalid task identities: %+v %+v", first, second)
			}

			if first.ManagerPID <= 0 || first.ManagerPID != second.ManagerPID {
				t.Fatalf("shells do not share a daemon: %d, %d", first.ManagerPID, second.ManagerPID)
			}
			cleanup := exec.Command(e.bin, "kill", "self:"+second.UID[:6])
			cleanup.Env = env
			if output, err := cleanup.CombinedOutput(); err != nil {
				t.Fatalf("cleanup: %s %v", output, err)
			}
		}
		if args[0] == "fixture-launch" && !strings.Contains(out, "Detached;") {
			t.Fatal(out)
		}
		if args[0] == "attach" && !strings.Contains(out, "Exited launch/renamed on local (code 0).") {
			t.Fatalf("missing shell exit notice: %s", out)
		}
	}
}

func closedAttachment(t *testing.T) *attachment {
	t.Helper()
	a, b := net.Pipe()
	client, err := smux.Client(a, wire.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	server, err := smux.Server(b, wire.MuxConfig())
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	stream, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	remote, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	server.Close()
	return newAttachment(server, remote, remote)
}
func TestInitialAttachmentFailureEndsCreatedTerminal(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	s := &remoteShell{name: "failed", done: make(chan struct{})}
	p := closedAttachment(t)
	if err := s.attach(p, message{Type: "attach", Term: "xterm-256color", Cols: 80, Rows: 24}); err == nil {
		t.Fatal("accepted closed attachment")
	}
	if s.terminal == nil {
		t.Fatal("test did not reach terminal startup")
	}
	defer s.terminal.Close()
	s.mu.Lock()
	exited := s.exited && s.active == nil
	s.mu.Unlock()
	if !exited {
		t.Fatal("initial failure preserved shell")
	}
	done := make(chan struct{})
	go func() { s.terminal.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = stopSpawnTerminal(s.process.PID, s.process.Start)
		t.Fatal("failed initial terminal survived")
	}
}
func TestFailedReattachmentPreservesDetachedTerminal(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	s := &remoteShell{name: "detached", done: make(chan struct{})}
	if err := s.start(message{Cols: 80, Rows: 24, Term: "xterm-256color"}); err != nil {
		t.Fatal(err)
	}
	defer func() { s.terminal.Close(); s.terminal.Wait() }()
	if err := s.attach(closedAttachment(t), message{Type: "attach", Term: "xterm-256color", Cols: 80, Rows: 24}); err == nil {
		t.Fatal("accepted closed attachment")
	}
	s.mu.Lock()
	detached := !s.exited && s.active == nil
	s.mu.Unlock()
	if !detached {
		t.Fatal("failed reconnect ended detached terminal")
	}
}

func TestDetachReportsFailedClientNotification(t *testing.T) {
	s := &remoteShell{done: make(chan struct{})}
	p := closedAttachment(t)
	s.active = p
	client, server := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	go s.handle(server)
	if err := writeMessage(client, message{Type: "detach"}); err != nil {
		t.Fatal(err)
	}
	reply, err := readMessage(client)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Type != "error" || reply.Error == "" {
		t.Fatalf("reported successful detach: %#v", reply)
	}
}

func TestPSTreeReattachmentAncestry(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("tree")
	shellOutput(t, b, "printf 'START-%s' ready", "START-ready")
	detachTestClient(t, b)
	for _, parent := range []string{"parent-a", "parent-b"} {
		b = reattachTestWithTree(t, e, "tree", pid, shellTreeNode{Identity: "origin", Host: "workstation", Name: "origin", State: "attached"}, shellTreeNode{Parent: "origin", Identity: parent, Host: "upstream", Name: parent, State: "attached"})
		output := shellOutput(t, b, shellQuote(e.bin)+" pstree; printf 'TREE-%s' done", "TREE-done")
		if !strings.Contains(output, "<- upstream") || !strings.Contains(output, "*current") || strings.Contains(output, "HOST  ") || strings.Contains(output, "("+parent+")") {
			t.Fatalf("incorrect local scope/source/current: %q", output)
		}
		if parent == "parent-b" && strings.Contains(output, "parent-a") {
			t.Fatalf("stale parent after attach: %q", output)
		}
		detachTestClient(t, b)
	}
	b, _ = e.attach("tree")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
	b.close()
}

func TestNoProfileRemoteStartup(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip(err)
	}
	e := newEnv(t)
	home := t.TempDir()
	for _, file := range []string{".bash_profile", ".bashrc"} {
		if err := os.WriteFile(filepath.Join(home, file), []byte("exit 77\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e.env = append(e.env, "SHELL="+bash, "HOME="+home, "SSHORE_PROFILE_INHERITED=kept")
	b := e.bridge("clean")
	defer b.close()
	sendTest(t, b.in, message{Type: "attach", Term: "xterm", Cols: 80, Rows: 24, NoProfile: true})
	m := recv(t, b.out)
	if m.Type != "ready" {
		t.Fatal(m)
	}
	process, err := os.FindProcess(m.PID)
	if err != nil {
		t.Fatal(err)
	}
	e.processes = append(e.processes, process)
	b.tty, err = b.mux.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if head := recv(t, b.tty); head.Type != "terminal" {
		t.Fatal(head)
	}
	shellOutput(t, b, `printf 'PROFILE-%s' "$SSHORE_PROFILE_INHERITED"`, "PROFILE-kept")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
}

func TestPSTreeKeepsUnavailableHostedShell(t *testing.T) {
	dir, err := os.MkdirTemp("", "sshore-tree-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("SSHORE_RUNTIME_DIR", dir)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(dir, "state"))
	uid := newTaskUID()
	tasks := startRequestDaemon(t)
	shell := &remoteShell{tasks: tasks, instance: uid, name: "blocked", lifetime: awaitingSpawnOwner, done: make(chan struct{})}
	if err := tasks.register(uid, shell); err != nil {
		t.Fatal(err)
	}
	// A task's status can fail without erasing its known identity.
	saveShellTree(remoteTreeIdentity(uid), "blocked", nil, "detached", "heartbeat")
	// The listening socket never answers, but its local lifetime lock stays held.
	inv, err := localTreeInventory()
	if err != nil {
		t.Fatal(err)
	}
	nodes := localShellTreeNodes(inv)
	var out bytes.Buffer
	writeShellTree(&out, nodes, inv.MachineID, false)
	if !strings.Contains(out.String(), uid[:6]+" (blocked)") || !strings.Contains(out.String(), "spawn/") {
		t.Fatal(out.String())
	}
}

func TestLocalDaemonOwnsForwardTransports(t *testing.T) {
	e := newEnv(t)
	state := filepath.Join(e.dir, "shared-forward")
	t.Cleanup(func() {
		deadline := time.Now().Add(35 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(state, "daemon", "daemon.json")); os.IsNotExist(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("local daemon survived idle shutdown after all tasks ended")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	env := append(e.env, "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=", "SSHORE_SSH=/missing/initial-ssh")
	run := func(environment []string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = environment
		cmd.Dir = e.dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		return string(out)
	}
	run(env, "fixture-launch", "--local", "keeper", "--detach")
	defer run(env, "kill", "self:keeper")
	raw, err := os.ReadFile(filepath.Join(state, "daemon", "daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manager localShellRecord
	if err = json.Unmarshal(raw, &manager); err != nil {
		t.Fatal(err)
	}
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "forward-ok")
			c.Close()
		}
	}()
	for _, name := range []string{"first", "second"} {
		observation := filepath.Join(e.dir, name+"-parent")
		stub := filepath.Join(e.dir, name+"-ssh")
		script := "#!/bin/sh\nprintf 'sshore-forward-ready\n'\nexec cat >/dev/null\n"
		if err = os.WriteFile(stub, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		stub = sshtest.Wrap(t, stub, env)
		probe, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := fmt.Sprint(probe.Addr().(*net.TCPAddr).Port)
		probe.Close()
		out := run(append(append([]string(nil), env...), "SSHORE_TEST_CONFIG_OWNER="+observation, "SSHORE_SSH="+stub), "forward", ".:"+port, "fixture:"+fmt.Sprint(target.Addr().(*net.TCPAddr).Port))
		conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", port), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		data := make([]byte, len("forward-ok"))
		_, err = io.ReadFull(conn, data)
		conn.Close()
		if err != nil || string(data) != "forward-ok" {
			t.Fatalf("forwarded bytes: %q %v", data, err)
		}

		_, started, ok := strings.Cut(out, "Started forward/")
		if !ok {
			t.Fatalf("missing auto-assigned forward name: %s", out)
		}
		assigned, _, ok := strings.Cut(started, ":")
		if !ok || validSession(assigned) != nil {
			t.Fatalf("invalid forward name: %s", out)
		}
		defer run(env, "kill", "forward/"+assigned)
		raw, err = os.ReadFile(observation)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != fmt.Sprintf("%d\n%s\n", manager.ManagerPID, e.dir) {
			t.Fatalf("forward has a wrapper process or lost cwd: %s", raw)
		}
	}
}

func TestLocalDaemonIgnoresOrphanedVersion(t *testing.T) {
	e := newEnv(t)
	for _, old := range []string{"0.0.1", "999.0.0", "unknown"} {
		t.Run(old, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			t.Setenv("SSHORE_STATE_DIR", state)
			path, err := daemonPath()
			if err != nil {
				t.Fatal(err)
			}
			r := localShellRecord{Name: "daemon", Address: "127.0.0.1:1", Token: strings.Repeat("a", 64), Version: old}
			data, _ := json.Marshal(r)
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			// A live owner must still reject the mismatch, even if it cannot answer.
			owner, err := daemonFileLock("owner")
			if err != nil {
				t.Fatal(err)
			}
			_, err = ensureLocalDaemonContext(context.Background())
			owner.Close()
			if err == nil {
				t.Fatal("live incompatible owner was replaced")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			env := append(e.env, "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=")
			cmd := exec.CommandContext(ctx, e.bin, "fixture-spawn", "--local", "fresh", "--detach")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("orphaned record blocked spawn: %v %s", err, out)
			}
			defer func() {
				cmd := exec.CommandContext(ctx, e.bin, "kill", "--all")
				cmd.Env, cmd.Stdin = env, strings.NewReader("kill\n")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("cleanup: %v %s", err, out)
				}
			}()
			current, err := readLocalRecord(path, "daemon")
			if err != nil || current.Version != version || current.Token == r.Token {
				t.Fatalf("daemon was not replaced: %+v %v", current, err)
			}
		})
	}
}

type trackedShellInput struct {
	stops int
}

func (i *trackedShellInput) Write(b []byte) (int, error) { return len(b), nil }
func (i *trackedShellInput) Stopped() bool               { return i.stops != 0 }
func (i *trackedShellInput) Stop() error {
	i.stops++
	return nil
}

func TestShellTerminalReleasesWriterAfterExit(t *testing.T) {
	var host shellTerminal
	active, stale := new(attachment), new(attachment)
	input := new(trackedShellInput)
	active.input = input
	host.active = active
	if detached, err := host.detachTerminal(stale); detached || err != nil || input.Stopped() {
		t.Fatal("stale attachment affected the active writer")
	}
	host.endTerminal()
	if detached, err := host.detachTerminal(active); detached || err != nil || input.stops != 1 {
		t.Fatal("exited terminal's writer was not stopped")
	}
}

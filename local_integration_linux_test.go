package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	transportio "github.com/jay-waves/sshore/internal/transport"
	"github.com/xtaci/smux"
)

func TestLocalShellDetachAndReattach(t *testing.T) {
	e := newEnv(t)
	state := filepath.Join(e.dir, "local-state")
	env := append(e.env, "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSH_CONNECTION=", "SHELL=/bin/sh", "TERM=xterm-256color")
	var id uint64
	for _, args := range [][]string{{"launch", "--local", "localtest"}, {"attach", "--local", "localtest"}} {
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
			id = rows[0].ID
			rename := exec.Command(e.bin, "rename", strconv.FormatUint(id, 10), "renamed")
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
			if err = json.Unmarshal(data, &rows); err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Name != "renamed" {
				t.Fatalf("renamed list: %s %v", data, err)
			}
			args = []string{"attach", strconv.FormatUint(id, 10)}
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
				if stage == 0 && strings.Contains(out.String(), "Connected local /") {
					stage = 1
					if args[0] == "launch" {
						io.WriteString(terminal, "\x02c")
					} else {
						io.WriteString(terminal, "exit\n")
					}
				}
				if args[0] == "launch" && stage == 1 && strings.Contains(out.String(), "Connected local / pikachu") {
					stage = 2
					io.WriteString(terminal, "\x02l")
				}
				if args[0] == "launch" && stage == 2 && strings.Count(out.String(), "Connected local / localtest") >= 2 {
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
		if args[0] == "launch" {
			first, err := readLocalRecord(filepath.Join(state, "local-shells", "localtest.json"), "localtest")
			if err != nil {
				t.Fatal(err)
			}
			second, err := readLocalRecord(filepath.Join(state, "local-shells", "pikachu.json"), "pikachu")
			if err != nil {
				t.Fatal(err)
			}
			if first.ManagerPID <= 0 || first.ManagerPID != second.ManagerPID {
				t.Fatalf("shells do not share a daemon: %d, %d", first.ManagerPID, second.ManagerPID)
			}
			cleanup := exec.Command(e.bin, "cancel", "--local", "pikachu")
			cleanup.Env = env
			if output, err := cleanup.CombinedOutput(); err != nil {
				t.Fatalf("cleanup: %s %v", output, err)
			}
		}
		if args[0] == "launch" && !strings.Contains(out, "Detached;") {
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
	client, err := smux.Client(a, transportio.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	server, err := smux.Server(b, transportio.MuxConfig())
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
	s := &remoteShell{name: "failed", plain: true, done: make(chan struct{})}
	p := closedAttachment(t)
	if err := s.attach(p, message{Type: "attach", Term: "xterm-256color", Cols: 80, Rows: 24}); err == nil {
		t.Fatal("accepted closed attachment")
	}
	if s.master == nil {
		t.Fatal("test did not reach terminal startup")
	}
	defer s.master.Close()
	if !s.lifecycle.Exited() || s.lifecycle.Active() != nil {
		t.Fatal("initial failure preserved shell")
	}
	done := make(chan struct{})
	go func() { s.master.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		s.cmd.Process.Kill()
		t.Fatal("failed initial terminal survived")
	}
}
func TestFailedReattachmentPreservesDetachedTerminal(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	s := &remoteShell{name: "detached", plain: true, done: make(chan struct{})}
	if err := s.start(80, 24, "xterm-256color"); err != nil {
		t.Fatal(err)
	}
	defer func() { s.master.Close(); s.master.Wait() }()
	if err := s.attach(closedAttachment(t), message{Type: "attach", Term: "xterm-256color", Cols: 80, Rows: 24}); err == nil {
		t.Fatal("accepted closed attachment")
	}
	if s.lifecycle.Exited() || s.lifecycle.Active() != nil {
		t.Fatal("failed reconnect ended detached terminal")
	}
}

func TestDetachReportsFailedClientNotification(t *testing.T) {
	s := &remoteShell{done: make(chan struct{})}
	p := closedAttachment(t)
	if err := s.lifecycle.Attach(p, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
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
		b = approvedTestAttach(t, e, "tree", pid, shellTreeNode{Identity: "origin", Host: "workstation", Name: "origin", State: "attached"}, shellTreeNode{Parent: "origin", Identity: parent, Host: "upstream", Name: parent, State: "attached"})
		resumeTestOutput(t, b)
		output := shellOutput(t, b, shellQuote(e.bin)+" pstree; printf 'TREE-%s' done", "TREE-done")
		if !strings.Contains(output, parent) || !strings.Contains(output, "← current") || !strings.Contains(output, "workstation / origin") {
			t.Fatalf("missing ancestry/current: %q", output)
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

func TestLocalNoLoginStartup(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	e := newEnv(t)
	home := t.TempDir()
	marker := filepath.Join(home, "startup")
	for file, value := range map[string]string{".bash_profile": "login", ".bashrc": "interactive"} {
		if err := os.WriteFile(filepath.Join(home, file), []byte("printf "+value+" > "+shellQuote(marker)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	env := append(e.env, "HOME="+home, "SHELL="+bash, "SSHORE_SOCKET=", "SSHORE_STATE_DIR="+filepath.Join(e.dir, "local-state"))
	run := func(args ...string) {
		t.Helper()
		c := exec.Command(e.bin, args...)
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	run("launch", "--local", "startup", "--no-login", "--detach")
	defer run("cancel", "--local", "startup")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			if string(data) != "interactive" {
				t.Fatalf("wrong startup: %q", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
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

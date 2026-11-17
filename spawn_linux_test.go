//go:build linux && sshoretest

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/jay-waves/sshore/internal/sshtest"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpawnTransportDrainsExistingStreamsAfterShellExit(t *testing.T) {
	dir, err := os.MkdirTemp("", "ss-drain-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "shell.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	owner, lease := net.Pipe()
	defer owner.Close()
	defer lease.Close()
	go func() {
		if m, err := readMessage(owner); err != nil || m.Type != "spawn_owner" {
			owner.Close()
			return
		}
		if writeMessage(owner, message{Type: "spawn_owned"}) != nil {
			owner.Close()
		}
	}()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- bridgeSpawnTransport(path, lease, "", server) }()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	mx, err := clientMux(client)
	if err != nil {
		t.Fatal(err)
	}
	defer mx.Close()
	stream, err := mx.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	exchange := func(c io.ReadWriter) error {
		if _, err := c.Write([]byte("x")); err != nil {
			return err
		}
		_, err := io.ReadFull(c, make([]byte, 1))
		return err
	}
	stream.SetDeadline(time.Now().Add(5 * time.Second))
	if err := exchange(stream); err != nil {
		t.Fatal(err)
	}
	// Each accepted stream opens its own data connection; a busy bridge must
	// grow beyond the former fixed eight-stream ceiling.
	var extraStreams []io.Closer
	for range 12 {
		extra, err := mx.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		extraStreams = append(extraStreams, extra)
		defer extra.Close()
		extra.SetDeadline(time.Now().Add(5 * time.Second))
		if err := exchange(extra); err != nil {
			t.Fatal(err)
		}
	}
	owner.Close()
	// Observe rejection of new streams before testing the retained stream.
	for deadline := time.Now().Add(3 * time.Second); ; {
		probe, err := mx.OpenStream()
		if err != nil {
			break
		}
		probe.SetDeadline(deadline)
		err = exchange(probe)
		probe.Close()
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ended shell still accepts streams")
		}
	}
	if err := exchange(stream); err != nil {
		t.Fatalf("existing resource stream lost with shell: %v", err)
	}
	stream.Close()
	for _, extra := range extraStreams {
		extra.Close()
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drained transport did not close")
	}
}

func TestSpawnLifetime(t *testing.T) {
	for _, tc := range []struct {
		remote  bool
		command string
	}{{false, "spawn"}, {true, "spawn"}} {
		remote := tc.remote
		t.Run(fmt.Sprintf("%s/remote=%t", tc.command, remote), func(t *testing.T) {
			e := newEnv(t)
			state := filepath.Join(e.dir, "spawn-state")
			home := filepath.Join(e.dir, "home")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			env := append(e.env, "SSHORE_STATE_DIR="+state, "HOME="+home, "SSHORE_DISABLED=", "SSHORE_LOCAL_SESSION=", "SSHORE_SOCKET=", "SSHORE_TREE_ID=original")
			t.Setenv("SSHORE_STATE_DIR", state)
			saveShellTree("original", "work", nil, "attached", "persistent")
			args := []string{tc.command, "--local", "temporary", "--detach"}
			if remote {
				ssh := filepath.Join(e.dir, "ssh")
				if err := os.WriteFile(ssh, []byte("#!/bin/sh\nunset SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN SSH_ASKPASS\nfor arg do last=$arg; done\nexec sh -c \"$last\"\n"), 0700); err != nil {
					t.Fatal(err)
				}
				ssh = sshtest.Wrap(t, ssh, env)
				env = append(env, "SSHORE_SSH="+ssh)
				args = []string{tc.command, "fake-host", "temporary", "--detach", "--server", e.bin}
			}
			// This parent stays alive after the detached CLI exits, then exits on EOF.
			parent := exec.Command("sh", append([]string{"-c", `"$@"; result=$?; printf 'READY:%s\n' "$result"; read answer`, "parent", e.bin}, args...)...)
			parent.Env = env
			input, err := parent.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := parent.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			parent.Stderr = &stderr
			if err = parent.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { input.Close(); parent.Process.Kill(); parent.Wait() })
			lines := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(output)
				var b strings.Builder
				for scanner.Scan() {
					b.WriteString(scanner.Text() + "\n")
					if strings.HasPrefix(scanner.Text(), "READY:") {
						lines <- b.String()
						return
					}
				}
				lines <- b.String()
			}()
			select {
			case out := <-lines:
				if !strings.Contains(out, "Started spawn/temporary") || !strings.Contains(out, "READY:0") {
					t.Fatalf("startup: %s\n%s", out, stderr.String())
				}
			case <-time.After(20 * time.Second):
				t.Fatal("startup timeout")
			}
			inspect := func() ([]byte, error) {
				args := []string{"list", "--local", "--json"}
				if remote {
					args = []string{"list", "fake-host", "--json", "--server", e.bin}
				}
				cmd := exec.Command(e.bin, args...)
				cmd.Env = env
				return cmd.CombinedOutput()
			}
			out, err := inspect()
			if err != nil {
				t.Fatalf("list: %s %v", out, err)
			}
			var pid int
			if remote {
				var inv inventory
				if err = json.Unmarshal(out, &inv); err != nil {
					t.Fatalf("%s: %v", out, err)
				}
				if len(inv.Shells) != 1 || !inv.Shells[0].Temporary || inv.Shells[0].Lifetime != "heartbeat" || inv.Shells[0].State != "detached" {
					t.Fatalf("unexpected shells: %s", out)
				}
				pid = inv.Shells[0].PID
			} else {
				var rows []localShellView
				if err = json.Unmarshal(out, &rows); err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || !rows[0].Temporary || rows[0].Lifetime != "parent" || rows[0].State != "detached" {
					t.Fatalf("unexpected shells: %s", out)
				}
				pid = rows[0].PID
			}
			if pid <= 0 {
				t.Fatal("spawn did not start its shell")
			}
			// Reattach/detach over the regular endpoint; the original parent still owns it.
			var conn io.ReadWriteCloser
			if remote {
				t.Setenv("SSHORE_RUNTIME_DIR", e.dir)
				path, err := resolveSession("temporary")
				if err != nil {
					t.Fatal(err)
				}
				c, err := dialHostedTask(context.Background(), path, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				conn = c
			} else {
				t.Setenv("SSHORE_STATE_DIR", state)
				r, err := readLocalSession("temporary")
				if err != nil {
					t.Fatal(err)
				}
				c, err := dialLocalSession(r)
				if err != nil {
					t.Fatal(err)
				}
				conn = c
			}
			mx, err := clientMux(conn)
			if err != nil {
				t.Fatal(err)
			}
			control, err := mx.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			if err = writeMessage(control, message{Type: "attach", Tree: []shellTreeNode{{Identity: "other", Host: "elsewhere", Name: "other"}}, Cols: 80, Rows: 24, Term: "xterm"}); err != nil {
				t.Fatal(err)
			}
			ready, err := readMessage(control)
			if err != nil || ready.Type != "ready" || !ready.Temporary || ready.PID != pid {
				t.Fatalf("reattach: %+v %v", ready, err)
			}
			if len(ready.Tree) == 0 {
				t.Fatal("missing relationship metadata")
			}
			self := ready.Tree[len(ready.Tree)-1]
			if self.Owner != "original" || self.Parent != "other" || self.Lifetime != shellLifetime(true, remote) {
				t.Fatalf("reattach changed lifetime owner: %+v", ready.Tree)
			}
			terminal, err := mx.AcceptStream()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = readMessage(terminal); err != nil {
				t.Fatal(err)
			}
			go io.Copy(io.Discard, terminal)
			if err = detachControl(control); err != nil {
				t.Fatal(err)
			}
			mx.Close()
			conn.Close()
			input.Close()
			_ = parent.Wait()
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := spawnProcessIdentity(pid); err != nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatalf("spawn shell %d survived parent exit", pid)
		})
	}
}
